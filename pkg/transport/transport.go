// Package transport implements the HTTP layer for the alertmanager webhook bridge.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/alertmanager/notify/webhook"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/trace"

	"github.com/aursu/prometheus-msteams/pkg/service"
	"github.com/labstack/echo/v4"
)

// Route holds the Service implementation and the Request path to serve the Service.
type Route struct {
	Service     service.Service
	RequestPath string
}

// NewServer creates the web server. It serves exactly:
//   - POST on each route's RequestPath: an Alertmanager webhook message,
//     handed to that route's Service
//   - GET /-/healthy and GET /-/ready: health checks for Kubernetes probes
//
// (main adds GET /metrics.) Any other path is 404; another method on a
// route path is 405.
func NewServer(logger log.Logger, routes []Route) *echo.Echo {
	e := echo.New()
	for _, r := range routes {
		level.Debug(logger).Log("request_path_added", r.RequestPath)
		addRoute(e, r.RequestPath, r.Service, logger)
	}
	addHealthChecks(e)
	e.HideBanner = true
	return e
}

// addHealthChecks registers the liveness and readiness endpoints, named as
// in Prometheus and Alertmanager. Both always answer 200: the configuration
// is read once at startup and an invalid one stops the program, so a process
// that answers HTTP is both alive and ready.
func addHealthChecks(e *echo.Echo) {
	ok := func(c echo.Context) error {
		return c.String(http.StatusOK, "OK")
	}
	e.GET("/-/healthy", ok)
	e.GET("/-/ready", ok)
}

func opencensusMiddleware() echo.MiddlewareFunc {
	return echo.WrapMiddleware(func(h http.Handler) http.Handler {
		return &ochttp.Handler{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r)
			}),
		}
	})
}

func kitLoggerMiddleware(logger log.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			defer func(begin time.Time) {
				res := c.Response()
				req := c.Request()
				logger.Log(
					"method", req.Method,
					"uri", req.RequestURI,
					"host", req.Host,
					"status", res.Status,
					"took", time.Since(begin),
				)
			}(time.Now())
			return next(c)
		}
	}
}

func addRoute(e *echo.Echo, p string, s service.Service, logger log.Logger) {
	e.POST(p, func(c echo.Context) error {
		return handleRoute(c, s, logger)
	},
		kitLoggerMiddleware(logger),
		opencensusMiddleware(),
	)
}

// handleRoute reads the Alertmanager message from the request and passes it
// to the Service. The status code tells Alertmanager what to do next: it
// retries a 5xx later, but not a 4xx (it counts that notification as
// failed). A malformed request is answered 400, since sending the same bytes
// again cannot succeed; a failed delivery as statusFor decides.
func handleRoute(c echo.Context, s service.Service, logger log.Logger) error {
	ctx, span := trace.StartSpan(c.Request().Context(), "alertmanager-handler")
	defer span.End()

	b, err := io.ReadAll(c.Request().Body)
	if err != nil {
		logger.Log("err", err)
		span.SetStatus(trace.Status{Code: 500, Message: err.Error()})
		return c.String(500, err.Error())
	}

	span.AddAttributes(trace.StringAttribute("alert", string(b)))

	var wm webhook.Message
	if err := json.Unmarshal(b, &wm); err != nil {
		logger.Log("err", err)
		span.SetStatus(trace.Status{Code: http.StatusBadRequest, Message: err.Error()})
		return c.String(http.StatusBadRequest, err.Error())
	}

	if wm.Data == nil || wm.Version == "" || wm.GroupKey == "" {
		err = fmt.Errorf("the webhook message does not seem to be a valid Prometheus Alertmanager webhook. More information see https://prometheus.io/docs/alerting/latest/configuration/#webhook_config")
		logger.Log("err", err)
		span.SetStatus(trace.Status{Code: http.StatusBadRequest, Message: err.Error()})
		return c.String(http.StatusBadRequest, err.Error())
	}

	prs, err := s.Post(ctx, wm)
	if err != nil {
		status := statusFor(err)
		logger.Log("err", err, "status", status)
		span.SetStatus(trace.Status{Code: int32(status), Message: err.Error()})
		// Alertmanager logs this text; it never contains the webhook URL.
		return c.String(status, err.Error())
	}

	return c.JSON(http.StatusOK, prs)
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
