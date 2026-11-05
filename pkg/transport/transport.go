// Package transport implements the HTTP layer for the alertmanager webhook bridge.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/aursu/prometheus-msteams/pkg/service"
)

// Route holds the Service implementation and the Request path to serve the Service.
type Route struct {
	Service     service.Service
	RequestPath string
}

// Metrics of the webhook requests from Alertmanager, served on /metrics.
// promauto registers each metric with Prometheus' default registry when the
// program starts. The "code" label is the answer: 200 delivered, 422 / 502 /
// 504 failed (see statusFor), 400 not an Alertmanager message.
var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "prometheus_msteams_requests_total",
		Help: "Webhook requests from Alertmanager, by request path and answered status code.",
	}, []string{"handler", "code"})
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "prometheus_msteams_request_duration_seconds",
		Help:    "Time to answer a webhook request from Alertmanager, Teams included, by request path.",
		Buckets: prometheus.DefBuckets,
	}, []string{"handler"})
)

// withMetrics counts the requests to one webhook path and times them, with
// the helpers of the Prometheus client library. MustCurryWith fixes the
// "handler" label to the path; the helpers fill in "code" themselves.
func withMetrics(path string, next http.Handler) http.Handler {
	labels := prometheus.Labels{"handler": path}
	return promhttp.InstrumentHandlerCounter(requestsTotal.MustCurryWith(labels),
		promhttp.InstrumentHandlerDuration(requestDuration.MustCurryWith(labels), next))
}

// NewServer creates the web server. It serves exactly:
//   - POST on each route's RequestPath: an Alertmanager webhook message,
//     handed to that route's Service
//   - GET /-/healthy and GET /-/ready: health checks for Kubernetes probes
//
// (main adds GET /metrics.) Any other path is 404; another method on a
// served path is 405.
//
// The routing is Go's own http.ServeMux: since Go 1.22 its patterns name the
// method too ("POST /alertmanager"), and it answers 404 and 405 by itself.
// The caller can add more patterns to the returned mux.
func NewServer(logger log.Logger, routes []Route) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	for _, r := range routes {
		if err := checkRequestPath(r.RequestPath); err != nil {
			return nil, err
		}
		level.Debug(logger).Log("request_path_added", r.RequestPath)
		// Each layer wraps the next one: the log line is written after the
		// request went through the metrics and the handler.
		h := withRequestLog(logger, withMetrics(r.RequestPath, alertHandler(r.Service, logger)))
		mux.Handle(http.MethodPost+" "+r.RequestPath, h)
	}
	addHealthChecks(mux)
	return mux, nil
}

// checkRequestPath rejects a request path that http.ServeMux would read as
// something else: it must start with "/", and "{...}" would be a wildcard.
func checkRequestPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("request path %q does not start with /", p)
	}
	if strings.ContainsAny(p, "{} ") {
		return fmt.Errorf("request path %q contains a brace or a space", p)
	}
	return nil
}

// addHealthChecks registers the liveness and readiness endpoints, named as
// in Prometheus and Alertmanager. Both always answer 200: the configuration
// is read once at startup and an invalid one stops the program, so a process
// that answers HTTP is both alive and ready.
func addHealthChecks(mux *http.ServeMux) {
	ok := func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, "OK")
	}
	mux.HandleFunc("GET /-/healthy", ok)
	mux.HandleFunc("GET /-/ready", ok)
}

// statusRecorder passes everything through to the real ResponseWriter and
// remembers the status code, which http.ResponseWriter cannot tell
// afterwards. (The ResponseWriter field has no name: Go "embeds" it, so all
// its methods are methods of statusRecorder too, and only WriteHeader is
// replaced here.)
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withRequestLog writes one log line per request, after it was handled.
func withRequestLog(logger log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begin := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Log(
			"method", r.Method,
			"uri", r.RequestURI,
			"host", r.Host,
			"status", rec.status,
			"took", time.Since(begin),
		)
	})
}

// alertHandler reads the Alertmanager message from the request and passes it
// to the Service. The status code tells Alertmanager what to do next: it
// retries a 5xx later, but not a 4xx (it counts that notification as
// failed). A malformed request is answered 400, since sending the same bytes
// again cannot succeed; a failed delivery as statusFor decides.
func alertHandler(s service.Service, logger log.Logger) http.Handler {
	// A function literal with the right signature becomes an http.Handler
	// through the conversion http.HandlerFunc(...).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Log("err", err)
			reply(w, http.StatusInternalServerError, err.Error())
			return
		}

		var wm webhook.Message
		if err := json.Unmarshal(b, &wm); err != nil {
			logger.Log("err", err)
			reply(w, http.StatusBadRequest, err.Error())
			return
		}

		if wm.Data == nil || wm.Version == "" || wm.GroupKey == "" {
			err = fmt.Errorf("the webhook message does not seem to be a valid Prometheus Alertmanager webhook. More information see https://prometheus.io/docs/alerting/latest/configuration/#webhook_config")
			logger.Log("err", err)
			reply(w, http.StatusBadRequest, err.Error())
			return
		}

		prs, err := s.Post(r.Context(), wm)
		if err != nil {
			status := statusFor(err)
			logger.Log("err", err, "status", status)
			// Alertmanager logs this text; it never contains the webhook URL.
			reply(w, status, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(prs); err != nil {
			logger.Log("err", err)
		}
	})
}

// reply answers with a status code and a line of plain text.
func reply(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// statusFor chooses the answer to Alertmanager for a failed delivery:
//
//	422 Unprocessable Entity  the card cannot be built, or Teams refused
//	                          it for good (4xx except 429: Workflow deleted,
//	                          card rejected, too large) - do not retry
//	504 Gateway Timeout       Teams did not answer in time - retry later
//	502 Bad Gateway           Teams answered 429 or 5xx, or could not be
//	                          reached - retry later
//
// 422 rather than Teams' own 4xx: a 404 from this service would read as
// "wrong path on this service".
func statusFor(err error) int {
	if errors.Is(err, service.ErrCard) {
		return http.StatusUnprocessableEntity
	}
	var te *service.TeamsError
	if errors.As(err, &te) {
		if te.Retryable() {
			return http.StatusBadGateway
		}
		return http.StatusUnprocessableEntity
	}
	// A timeout is either the context deadline or a network error that
	// says so: such errors have a method Timeout() bool. The anonymous
	// interface type below matches any error with that method.
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
