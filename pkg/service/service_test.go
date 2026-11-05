package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
// when the test ends. The handler runs in the server's goroutine, so the
// count is an atomic.Int32, safe to read from the test's goroutine.
func fakeTeams(t *testing.T, status int, body string) (webhookURL string, calls *atomic.Int32) {
	t.Helper()
	calls = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/workflows/x/triggers/manual/paths/invoke?sig=" + testSig, calls
}

// silentTeams plays a Teams that accepts the request but never answers: each
// request waits until the client gives up.
func silentTeams(t *testing.T) (webhookURL string, calls *atomic.Int32) {
	t.Helper()
	calls = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Go's server notices that the client went away (and cancels
		// r.Context()) only once the request body has been read to the end.
		_, _ = io.Copy(io.Discard, r.Body)
		// select waits for whichever comes first; the 5s case only keeps a
		// broken test from hanging.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/workflows/x/triggers/manual/paths/invoke?sig=" + testSig, calls
}

// retryClient returns a retry client configured as in main, with retryMax
// retries and a short wait between them.
func retryClient(retryMax int) *http.Client {
	rc := retryablehttp.NewClient()
	rc.Logger = nil
	rc.RetryMax = retryMax
	rc.RetryWaitMin = 10 * time.Millisecond
	rc.RetryWaitMax = 10 * time.Millisecond
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
	return rc.StandardClient()
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
			s := NewSimpleService(fixedCard{}, http.DefaultClient, u, 0)

			prs, err := s.Post(context.Background(), webhook.Message{})

			if calls.Load() != 1 {
				t.Fatalf("Teams called %d times, want 1", calls.Load())
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
	_, err := NewSimpleService(fixedCard{}, http.DefaultClient, u, 0).Post(context.Background(), webhook.Message{})

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

	_, err := NewSimpleService(fixedCard{}, retryClient(0), u, 0).Post(context.Background(), webhook.Message{})

	var te *TeamsError
	if !errors.As(err, &te) || te.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Post() error = %v, want *TeamsError 503", err)
	}
	if calls.Load() != 1 {
		t.Errorf("Teams called %d times, want 1 (no retries)", calls.Load())
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

	_, err := NewSimpleService(fixedCard{}, retryClient(0), u, 0).Post(context.Background(), webhook.Message{})
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
	_, err := NewSimpleService(fixedCard{err: errors.New("bad template")}, http.DefaultClient, u, 0).
		Post(context.Background(), webhook.Message{})

	if !errors.Is(err, ErrCard) || !strings.Contains(err.Error(), "bad template") {
		t.Fatalf("Post() error = %v, want ErrCard with the cause", err)
	}
	if calls.Load() != 0 {
		t.Errorf("Teams called %d times, want 0", calls.Load())
	}
}

// When Teams does not answer, Post gives up after the timeout - not after
// the retries: the limit covers the delivery as a whole, so a retry client
// allowed 3 retries still makes only the one attempt that hung.
func TestPost_timeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	u, calls := silentTeams(t)

	start := time.Now()
	_, err := NewSimpleService(fixedCard{}, retryClient(3), u, timeout).Post(context.Background(), webhook.Message{})
	took := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Post() error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.HasPrefix(err.Error(), "teams did not answer within 200ms") {
		t.Errorf("error %q does not name the timeout", err)
	}
	if took < timeout || took > 2*time.Second {
		t.Errorf("Post() took %s, want just over %s", took, timeout)
	}
	if calls.Load() != 1 {
		t.Errorf("Teams called %d times, want 1", calls.Load())
	}
	if strings.Contains(err.Error(), testSig) || strings.Contains(err.Error(), "/workflows/") {
		t.Errorf("error leaks the URL: %v", err)
	}
}
