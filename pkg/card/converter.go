// Package card provides types and utilities for converting Prometheus
// Alertmanager webhook messages into Microsoft Teams Adaptive Card messages.
package card

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/prometheus/alertmanager/notify/webhook"
)

// Converter turns one Alertmanager webhook message (one alert group) into one
// Teams Workflow message carrying an Adaptive Card.
// (A Go interface lists the methods a type must have; any type that has them
// satisfies the interface automatically, without an "implements" keyword.)
//
// The result is json.RawMessage: the finished JSON bytes, sent to Teams as
// they are. The card is deliberately not mapped onto Go structs, so that
// whatever the template writes - any Adaptive Card field, any version -
// reaches Teams unchanged.
type Converter interface {
	Convert(context.Context, webhook.Message) (json.RawMessage, error)
}

// loggingMiddleware wraps another Converter and logs every conversion
// (decorator pattern).
type loggingMiddleware struct {
	logger *slog.Logger
	next   Converter
}

// NewCreatorLoggingMiddleware creates a loggingMiddleware.
func NewCreatorLoggingMiddleware(l *slog.Logger, n Converter) Converter {
	return loggingMiddleware{l, n}
}

// Convert calls the wrapped Converter and logs, at debug level only, the
// alert, the resulting card and how long the conversion took: both hold the
// full alert texts, too much and too private for every log line. The named
// results (c, err) let the deferred function read them after the wrapped
// call has returned.
func (l loggingMiddleware) Convert(ctx context.Context, a webhook.Message) (c json.RawMessage, err error) {
	// defer runs this function when Convert returns, whichever way it does.
	defer func(begin time.Time) {
		l.logger.Debug("card built",
			"alert", a,
			"card", string(c),
			"err", err,
			"took", time.Since(begin),
		)
	}(time.Now())
	return l.next.Convert(ctx, a)
}
