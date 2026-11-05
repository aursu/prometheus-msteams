package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/prometheus/alertmanager/notify/webhook"
)

// testSig stands for the secret signature in a Workflow URL; no error text
// may contain it.
const testSig = "SECRET-SIGNATURE"

// fixedCard is a stand-in Converter: it returns the same small card, or err.
type fixedCard struct{ err error }

func (f fixedCard) Convert(context.Context, webhook.Message) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(`{"type":"message","attachments":[]}`), nil
}

// fakeTeams starts a local HTTP server that plays Teams: it answers every
// request with status and body, and counts the requests. Cleanup stops it
// when the test ends.
func fakeTeams(t *testing.T, status int, body string) (webhookURL string, calls *int) {
	t.Helper()
	calls = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/workflows/x/triggers/manual/paths/invoke?sig=" + testSig, calls
}

// Every Teams answer is classified: 2xx is delivered, anything else is a
// *TeamsError that says whether a later retry can help.
func TestPost_teamsAnswers(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantErr       bool
		wantRetryable bool
	}{
		{name: "accepted (what a Workflow answers)", status: http.StatusAccepted},
		{name: "ok", status: http.StatusOK, body: "1"},
		{name: "bad request: card rejected", status: http.StatusBadRequest, body: `{"error":"InvalidCard"}`, wantErr: true},
		{name: "not found: Workflow deleted", status: http.StatusNotFound, wantErr: true},
		{name: "payload too large", status: http.StatusRequestEntityTooLarge, wantErr: true},
		{name: "throttled", status: http.StatusTooManyRequests, wantErr: true, wantRetryable: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true, wantRetryable: true},
		{name: "service unavailable", status: http.StatusServiceUnavailable, wantErr: true, wantRetryable: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			u, calls := fakeTeams(t, tt.status, tt.body)
			s := NewSimpleService(fixedCard{}, http.DefaultClient, u)

			prs, err := s.Post(context.Background(), webhook.Message{})

			if *calls != 1 {
				t.Fatalf("Teams called %d times, want 1", *calls)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Post() error = %v, want none", err)
				}
				if len(prs) != 1 || prs[0].Status != tt.status || prs[0].Message != tt.body {
					t.Fatalf("Post() = %+v, want status %d and message %q", prs, tt.status, tt.body)
				}
				return
			}

			// errors.As: is err (or an error it wraps) a *TeamsError?
			var te *TeamsError
			if !errors.As(err, &te) {
				t.Fatalf("Post() error = %v (%T), want *TeamsError", err, err)
			}
			if te.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", te.StatusCode, tt.status)
			}
			if te.Retryable() != tt.wantRetryable {
				t.Errorf("Retryable() = %t, want %t", te.Retryable(), tt.wantRetryable)
			}
			if !strings.Contains(err.Error(), tt.body) {
				t.Errorf("error %q does not contain the Teams answer %q", err, tt.body)
			}
			if strings.Contains(err.Error(), testSig) {
				t.Errorf("error leaks the signature: %v", err)
			}
		})
	}
}

// A long Teams answer is cut to maxResponseBody bytes.
func TestPost_longAnswerIsCut(t *testing.T) {
	u, _ := fakeTeams(t, http.StatusBadRequest, strings.Repeat("x", 10*maxResponseBody))
	_, err := NewSimpleService(fixedCard{}, http.DefaultClient, u).Post(context.Background(), webhook.Message{})

	var te *TeamsError
	if !errors.As(err, &te) {
		t.Fatalf("Post() error = %v, want *TeamsError", err)
	}
	if len(te.Body) != maxResponseBody {
		t.Errorf("kept %d bytes of the answer, want %d", len(te.Body), maxResponseBody)
	}
}

// The program sends through a retry client (hashicorp/go-retryablehttp).
// Configured as in main - no retries, PassthroughErrorHandler - a 5xx must
// still arrive as *TeamsError, not as the client's "giving up" error that
// contains the full URL.
func TestPost_throughRetryClient(t *testing.T) {
	u, calls := fakeTeams(t, http.StatusServiceUnavailable, "busy")
	rc := retryablehttp.NewClient()
	rc.Logger = nil
	rc.RetryMax = 0
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler

	_, err := NewSimpleService(fixedCard{}, rc.StandardClient(), u).Post(context.Background(), webhook.Message{})

	var te *TeamsError
	if !errors.As(err, &te) || te.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Post() error = %v, want *TeamsError 503", err)
	}
	if *calls != 1 {
		t.Errorf("Teams called %d times, want 1 (no retries)", *calls)
	}
	if strings.Contains(err.Error(), testSig) {
		t.Errorf("error leaks the signature: %v", err)
	}
}

// When Teams cannot be reached at all, the error says so without the URL.
func TestPost_unreachable(t *testing.T) {
	// Start a server and stop it at once: its port is closed, so the
	// connection is refused.
	closed := httptest.NewServer(http.NotFoundHandler())
	u := closed.URL + "/workflows/x/triggers/manual/paths/invoke?sig=" + testSig
	closed.Close()

	rc := retryablehttp.NewClient()
	rc.Logger = nil
	rc.RetryMax = 0
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler

	_, err := NewSimpleService(fixedCard{}, rc.StandardClient(), u).Post(context.Background(), webhook.Message{})
	if err == nil {
		t.Fatal("Post() succeeded, want an error")
	}
	var te *TeamsError
	if errors.As(err, &te) {
		t.Errorf("Post() error = %v, want a connection error, not *TeamsError", err)
	}
	if !strings.HasPrefix(err.Error(), "sending to Teams failed: ") {
		t.Errorf("error %q does not say that sending failed", err)
	}
	if strings.Contains(err.Error(), testSig) || strings.Contains(err.Error(), "/workflows/") {
		t.Errorf("error leaks the URL: %v", err)
	}
}

// When the card cannot be built, nothing is sent.
func TestPost_cardFails(t *testing.T) {
	u, calls := fakeTeams(t, http.StatusAccepted, "")
	_, err := NewSimpleService(fixedCard{err: errors.New("bad template")}, http.DefaultClient, u).
		Post(context.Background(), webhook.Message{})

	if !errors.Is(err, ErrCard) || !strings.Contains(err.Error(), "bad template") {
		t.Fatalf("Post() error = %v, want ErrCard with the cause", err)
	}
	if *calls != 0 {
		t.Errorf("Teams called %d times, want 0", *calls)
	}
}
