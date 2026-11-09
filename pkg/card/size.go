package card

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
)

// DefaultMaxSize is the default limit for a card, in bytes. Teams refuses a
// Workflow message larger than about 28 KB (it answers 413); the limit stays
// below that, with room for what Teams adds or counts differently.
const DefaultMaxSize = 24 * 1024

// sizeLimiter wraps another Converter and keeps its cards within maxSize
// bytes (decorator pattern, like loggingMiddleware).
type sizeLimiter struct {
	logger  *slog.Logger
	next    Converter
	maxSize int
}

// NewSizeLimiter creates a sizeLimiter. maxSize 0 (or less) turns the limit
// off.
func NewSizeLimiter(l *slog.Logger, n Converter, maxSize int) Converter {
	return sizeLimiter{l, n, maxSize}
}

// Convert builds the card for the whole alert group. If it is larger than
// maxSize, it builds the card again with fewer alerts: as many as fit,
// firing alerts first. The alerts left out are added to the message's
// TruncatedAlerts, which a template can show as "N more alerts" (the default
// template does). Alertmanager uses the same field when its webhook
// "max_alerts" setting cuts a group.
//
// Without the limit Teams would refuse a large card with 413, Alertmanager
// does not retry a 4xx answer, and the notification would be lost. If even
// one alert does not fit, Convert returns an error.
func (s sizeLimiter) Convert(ctx context.Context, msg webhook.Message) (json.RawMessage, error) {
	c, err := s.next.Convert(ctx, msg)
	if err != nil || s.maxSize <= 0 || len(c) <= s.maxSize {
		return c, err
	}
	size := len(c)
	if msg.Data == nil || len(msg.Alerts) <= 1 {
		return nil, s.tooLarge(size)
	}

	alerts := firingFirst(msg.Alerts)

	// Binary search for the largest number of alerts whose card fits: every
	// round halves the range [lo, hi] that can still hold the answer, so a
	// group of 1000 alerts needs about 10 extra renderings. It relies on a
	// card growing with each alert, which it does in any normal template.
	var best json.RawMessage
	shown := 0
	lo, hi := 1, len(alerts)-1
	for lo <= hi {
		n := (lo + hi) / 2
		c, err := s.next.Convert(ctx, withAlerts(msg, alerts[:n], len(alerts)-n))
		if err != nil {
			return nil, err
		}
		if len(c) <= s.maxSize {
			best, shown = c, n
			lo = n + 1
		} else {
			hi = n - 1
		}
	}
	if best == nil {
		return nil, s.tooLarge(size)
	}

	s.logger.Warn("card too large, alerts left out",
		"alerts", len(alerts),
		"shown", shown,
		"size", size,
		"max_size", s.maxSize,
	)
	return best, nil
}

// tooLarge is the error when not even a card with one alert fits.
func (s sizeLimiter) tooLarge(size int) error {
	return fmt.Errorf("the card is %d bytes, more than the limit of %d, even with a single alert: "+
		"shorten the template or the alert's labels and annotations", size, s.maxSize)
}

// firingFirst returns a copy of alerts with the firing ones before the
// resolved ones; within each part the order stays as it was ("stable"
// sort). The input slice is not changed.
func firingFirst(alerts template.Alerts) template.Alerts {
	sorted := slices.Clone(alerts)
	// SortStableFunc takes a comparison function that returns a negative
	// number when a goes before b, a positive one when after, 0 when equal.
	slices.SortStableFunc(sorted, func(a, b template.Alert) int {
		return rank(a) - rank(b)
	})
	return sorted
}

// rank orders alerts: firing (0) before anything else (1).
func rank(a template.Alert) int {
	if a.Status == firing {
		return 0
	}
	return 1
}

// firing is the alert status Alertmanager sends for an active alert.
const firing = "firing"

// withAlerts returns a copy of msg that holds only the given alerts and
// counts omitted more alerts as truncated. msg is passed by value, so
// changing its fields here does not change the caller's message; msg.Data
// is a pointer, though, so the data is copied before Alerts is replaced.
func withAlerts(msg webhook.Message, alerts template.Alerts, omitted int) webhook.Message {
	d := *msg.Data
	d.Alerts = alerts
	msg.Data = &d
	msg.TruncatedAlerts += uint64(omitted) //nolint:gosec // omitted is never negative
	return msg
}
