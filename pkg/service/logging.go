// Package service implements the alert forwarding logic from Prometheus Alertmanager to MS Teams.
package service

import (
	"context"
	"log/slog"

	"github.com/prometheus/alertmanager/notify/webhook"
)

// loggingService is a logging middleware for Service.
type loggingService struct {
	logger *slog.Logger
	next   Service
}

// NewLoggingService creates a loggingService.
func NewLoggingService(logger *slog.Logger, next Service) Service {
	return loggingService{logger, next}
}

// Post calls the wrapped Service and logs Teams' answer at debug level.
func (s loggingService) Post(ctx context.Context, wm webhook.Message) (prs []PostResponse, err error) {
	defer func() {
		for _, pr := range prs {
			s.logger.Debug("teams answered",
				"response_message", pr.Message,
				"response_status", pr.Status,
				"err", err,
			)
		}
	}()
	return s.next.Post(ctx, wm)
}
