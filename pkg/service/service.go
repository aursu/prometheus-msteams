package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/prometheus-msteams/prometheus-msteams/pkg/card"
	"github.com/prometheus/alertmanager/notify/webhook"
	"go.opencensus.io/trace"
)

// Only one kind of Teams webhook is supported: a Teams Workflow ("Post to a
// channel when a webhook request is received"), which accepts Adaptive Cards
// only. Office 365 connectors (MessageCard format) were retired by Microsoft.

// PostResponse is the prometheus msteams service response.
type PostResponse struct {
	WebhookURL string `json:"webhook_url"`
	Status     int    `json:"status"`
	Message    string `json:"message"`
}

// Service is the Alertmanager to Microsoft Teams webhook service.
type Service interface {
	Post(context.Context, webhook.Message) (resp []PostResponse, err error)
}

// simpleService sends every alert group to one Teams Workflow URL.
// (In Go a struct is the data part of a class; the functions below declared
// with the receiver "(s simpleService)" are its methods.)
type simpleService struct {
	converter  card.Converter
	client     *http.Client
	webhookURL string
}

// NewSimpleService creates a simpleService. Go has no constructors; a
// New... function plays that role.
func NewSimpleService(converter card.Converter, client *http.Client, webhookURL string) Service {
	return simpleService{converter, client, webhookURL}
}

// Post renders the alert group into an Adaptive Card and sends it to Teams.
func (s simpleService) Post(ctx context.Context, wm webhook.Message) ([]PostResponse, error) {
	ctx, span := trace.StartSpan(ctx, "simpleService.Post")
	defer span.End()

	prs := []PostResponse{}

	c, err := s.converter.Convert(ctx, wm)
	if err != nil {
		return nil, fmt.Errorf("failed to parse webhook message: %w", err)
	}

	_, err = s.post(ctx, c, s.webhookURL)
	if err != nil {
		return prs, err
	}

	return prs, nil
}

func (s simpleService) post(ctx context.Context, c interface{}, url string) (PostResponse, error) {
	ctx, span := trace.StartSpan(ctx, "simpleService.post")
	defer span.End()

	pr := PostResponse{WebhookURL: url}

	b, err := json.Marshal(c)
	if err != nil {
		err = fmt.Errorf("failed to decoding JSON card: %w", err)
		return pr, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.webhookURL, bytes.NewBuffer(b))
	if err != nil {
		err = fmt.Errorf("failed to creating a request: %w", err)
		return pr, err
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		err = fmt.Errorf("http client failed: %w", err)
		return pr, err
	}
	defer func() { _ = resp.Body.Close() }()

	pr.Status = resp.StatusCode

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("failed reading http response body: %w", err)
		pr.Message = err.Error()
		return pr, err
	}
	pr.Message = string(rb)

	return pr, nil
}
