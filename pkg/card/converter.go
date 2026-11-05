// Package card provides types and utilities for converting Prometheus
// Alertmanager webhook messages into Microsoft Teams Adaptive Card messages.
package card

import (
	"context"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/alertmanager/notify/webhook"
)

// Action is one entry of an Adaptive Card "actions" list, e.g. an
// Action.OpenUrl button. Action types differ in their fields, so it is kept as
// a free-form JSON object (a map from field name to any value) instead of a
// fixed struct.
type Action map[string]interface{}

// Converter turns one Alertmanager webhook message (one alert group) into one
// Teams Workflow message carrying an Adaptive Card.
// (A Go interface lists the methods a type must have; any type that has them
// satisfies the interface automatically, without an "implements" keyword.)
type Converter interface {
	Convert(context.Context, webhook.Message) (WorkflowConnectorCard, error)
}

// loggingMiddleware wraps another Converter and logs every conversion
// (decorator pattern). Its Convert method is in converterWorkflow.go.
type loggingMiddleware struct {
	logger log.Logger
	next   Converter
}

// NewCreatorLoggingMiddleware creates a loggingMiddleware.
func NewCreatorLoggingMiddleware(l log.Logger, n Converter) Converter {
	return loggingMiddleware{l, n}
}
