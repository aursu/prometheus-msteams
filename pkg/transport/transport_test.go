package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/prometheus-msteams/prometheus-msteams/pkg/service"
	"github.com/prometheus/alertmanager/notify/webhook"
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
		{name: "service fails", method: http.MethodPost, path: webhookPath, body: string(validMessage), serviceErr: errors.New("teams said no"), wantStatus: http.StatusInternalServerError, wantCalls: 1},
		{name: "GET on the webhook path", method: http.MethodGet, path: webhookPath, wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path", method: http.MethodPost, path: "/other", body: string(validMessage), wantStatus: http.StatusNotFound},

		// Endpoints of the upstream program that must not exist any more.
		{name: "no dynamic webhook", method: http.MethodPost, path: "/_dynamicwebhook/example.org/x", body: string(validMessage), wantStatus: http.StatusNotFound},
		{name: "no config listing", method: http.MethodGet, path: "/config", wantStatus: http.StatusNotFound},
		{name: "no profiler", method: http.MethodGet, path: "/debug/pprof/", wantStatus: http.StatusNotFound},

		{name: "liveness", method: http.MethodGet, path: "/-/healthy", wantStatus: http.StatusOK},
		{name: "readiness", method: http.MethodGet, path: "/-/ready", wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{err: tt.serviceErr}
			srv := NewServer(log.NewNopLogger(), []Route{{Service: svc, RequestPath: webhookPath}})

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
