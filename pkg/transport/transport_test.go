package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aursu/prometheus-msteams/pkg/service"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeService stands in for the real Service (which renders a card and
// calls Teams): it counts calls and returns err. Its Post method makes it
// satisfy the service.Service interface.
type fakeService struct {
	calls int
	err   error
}

func (f *fakeService) Post(_ context.Context, _ webhook.Message) ([]service.PostResponse, error) {
	f.calls++
	return []service.PostResponse{}, f.err
}

// timeoutError imitates a network error that reports a timeout, like the
// one Go's HTTP client returns when its Timeout is exceeded.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }

// The HTTP layer answers each kind of request with the status code that
// tells Alertmanager what to do, and serves nothing beyond the configured
// webhook paths and the health checks.
func TestServer(t *testing.T) {
	// A real Alertmanager webhook message (has version, groupKey, alerts).
	validMessage, err := os.ReadFile("../card/testdata/prom_post_request.json")
	if err != nil {
		t.Fatal(err)
	}

	const webhookPath = "/alertmanager"
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		serviceErr error // what the fake service returns
		wantStatus int
		wantCalls  int // how often the service must have been called
	}{
		{name: "valid message", method: http.MethodPost, path: webhookPath, body: string(validMessage), wantStatus: http.StatusOK, wantCalls: 1},
		{name: "invalid JSON", method: http.MethodPost, path: webhookPath, body: `{"version": "4",`, wantStatus: http.StatusBadRequest},
		{name: "JSON that is not an Alertmanager message", method: http.MethodPost, path: webhookPath, body: `{"hello": "world"}`, wantStatus: http.StatusBadRequest},
		// A failed delivery: the answer tells Alertmanager whether to retry
		// (5xx) or not (4xx).
		{name: "card cannot be built", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: fmt.Errorf("%w: bad template", service.ErrCard), wantStatus: http.StatusUnprocessableEntity, wantCalls: 1},
		{name: "teams 404: Workflow deleted", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: &service.TeamsError{StatusCode: http.StatusNotFound}, wantStatus: http.StatusUnprocessableEntity, wantCalls: 1},
		{name: "teams 400: card rejected", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: &service.TeamsError{StatusCode: http.StatusBadRequest}, wantStatus: http.StatusUnprocessableEntity, wantCalls: 1},
		{name: "teams 413: card too large", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: &service.TeamsError{StatusCode: http.StatusRequestEntityTooLarge}, wantStatus: http.StatusUnprocessableEntity, wantCalls: 1},
		{name: "teams 429: throttled", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: &service.TeamsError{StatusCode: http.StatusTooManyRequests}, wantStatus: http.StatusBadGateway, wantCalls: 1},
		{name: "teams 503", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: &service.TeamsError{StatusCode: http.StatusServiceUnavailable}, wantStatus: http.StatusBadGateway, wantCalls: 1},
		{name: "teams not answering in time", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: fmt.Errorf("sending to Teams failed: %w", context.DeadlineExceeded), wantStatus: http.StatusGatewayTimeout, wantCalls: 1},
		{name: "network timeout", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: timeoutError{}, wantStatus: http.StatusGatewayTimeout, wantCalls: 1},
		{name: "teams unreachable", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: errors.New("sending to Teams failed: connection refused"), wantStatus: http.StatusBadGateway, wantCalls: 1},
		{name: "GET on the webhook path", method: http.MethodGet, path: webhookPath, wantStatus: http.StatusMethodNotAllowed},
		// No CORS preflight answer: browsers are not clients of this service.
		{name: "OPTIONS on the webhook path", method: http.MethodOptions, path: webhookPath, wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path", method: http.MethodPost, path: "/other", body: string(validMessage), wantStatus: http.StatusNotFound},
		{name: "trailing slash is another path", method: http.MethodPost, path: webhookPath + "/", body: string(validMessage), wantStatus: http.StatusNotFound},
		// http.ServeMux cleans the path: "//", "/./" or "/../" in it are
		// answered with a redirect to the clean path. 307 keeps the method
		// and the body, so a client that follows it still delivers.
		{name: "unclean path is redirected", method: http.MethodPost, path: "/" + webhookPath, body: string(validMessage), wantStatus: http.StatusTemporaryRedirect},

		// Endpoints of the upstream program that must not exist any more.
		{name: "no dynamic webhook", method: http.MethodPost, path: "/_dynamicwebhook/example.org/x", body: string(validMessage), wantStatus: http.StatusNotFound},
		{name: "no config listing", method: http.MethodGet, path: "/config", wantStatus: http.StatusNotFound},
		{name: "no profiler", method: http.MethodGet, path: "/debug/pprof/", wantStatus: http.StatusNotFound},

		{name: "liveness", method: http.MethodGet, path: "/-/healthy", wantStatus: http.StatusOK},
		{name: "readiness", method: http.MethodGet, path: "/-/ready", wantStatus: http.StatusOK},
		// A GET pattern in http.ServeMux serves HEAD too.
		{name: "liveness with HEAD", method: http.MethodHead, path: "/-/healthy", wantStatus: http.StatusOK},
		{name: "POST on a health check", method: http.MethodPost, path: "/-/healthy", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{err: tt.serviceErr}
			srv, err := NewServer(slog.New(slog.DiscardHandler), []Route{{Service: svc, RequestPath: webhookPath}})
			if err != nil {
				t.Fatal(err)
			}

			// httptest.NewRecorder captures the response; ServeHTTP runs
			// the request through the server without opening a port.
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if svc.calls != tt.wantCalls {
				t.Errorf("service called %d times, want %d", svc.calls, tt.wantCalls)
			}
		})
	}
}

// A request path that http.ServeMux would misread stops the program at
// startup.
func TestNewServer_rejectsPath(t *testing.T) {
	for _, p := range []string{"alertmanager", "/alerts/{team}", "/a b", ""} {
		if _, err := NewServer(slog.New(slog.DiscardHandler), []Route{{Service: &fakeService{}, RequestPath: p}}); err == nil {
			t.Errorf("NewServer() accepted request path %q", p)
		}
	}
}

// Each request to a webhook path is logged with the status it was answered.
func TestServer_logsStatus(t *testing.T) {
	var buf bytes.Buffer
	svc := &fakeService{err: &service.TeamsError{StatusCode: http.StatusNotFound}}
	srv, err := NewServer(slog.New(slog.NewTextHandler(&buf, nil)), []Route{{Service: svc, RequestPath: "/alertmanager"}})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := os.ReadFile("../card/testdata/prom_post_request.json")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/alertmanager", bytes.NewReader(msg))
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), "method=POST uri=/alertmanager") || !strings.Contains(buf.String(), "status=422") {
		t.Errorf("log does not show the request with status 422:\n%s", buf.String())
	}
}

// Every webhook request is counted by path and answered status, and timed.
func TestServer_metrics(t *testing.T) {
	const path = "/metrics-test" // a path of its own: the metrics are global
	svc := &fakeService{err: &service.TeamsError{StatusCode: http.StatusNotFound}}
	srv, err := NewServer(slog.New(slog.DiscardHandler), []Route{{Service: svc, RequestPath: path}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := os.ReadFile("../card/testdata/prom_post_request.json")
	if err != nil {
		t.Fatal(err)
	}

	srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, bytes.NewReader(msg)))

	// testutil reads the current value of a metric.
	if got := testutil.ToFloat64(requestsTotal.WithLabelValues(path, "422")); got != 1 {
		t.Errorf(`requests_total{handler=%q, code="422"} = %v, want 1`, path, got)
	}
	if got := testutil.CollectAndCount(requestDuration); got == 0 {
		t.Error("request_duration_seconds has no series")
	}
}
