package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aursu/prometheus-msteams/pkg/card"
	"github.com/prometheus/alertmanager/notify/webhook"
	"go.opencensus.io/trace"
)

// Only one kind of Teams webhook is supported: a Teams Workflow ("Post to a
// channel when a webhook request is received"), which accepts Adaptive Cards
// only. Office 365 connectors (MessageCard format) were retired by Microsoft.

// PostResponse is what Teams answered to a delivered card (a Workflow
// answers 202 Accepted). It deliberately has no URL field: the URL carries
// the signature, and this response goes back to Alertmanager.
type PostResponse struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// ErrCard marks a failure to build the card from the alert group (template
// error, output Teams would not accept). Nothing was sent, and the same
// alert group fails the same way again. Callers test for it with
// errors.Is(err, ErrCard).
var ErrCard = errors.New("failed to build the card")

// maxResponseBody is how much of a Teams response body is kept, in success
// and error alike; enough for an error message, bounded for logs.
const maxResponseBody = 512

// TeamsError means Teams answered, but not with 2xx. The caller decides by
// StatusCode (see Retryable) whether trying again later can help.
type TeamsError struct {
	StatusCode int
	Body       string // the first maxResponseBody bytes of the answer
}

// Error makes TeamsError satisfy Go's built-in error interface, which
// consists of this one method.
func (e *TeamsError) Error() string {
	return fmt.Sprintf("teams answered %d %s: %s", e.StatusCode, http.StatusText(e.StatusCode), e.Body)
}

// Retryable reports whether the same card may succeed later: throttling
// (429) and server errors (5xx). Any other 4xx - Workflow deleted (404),
// card rejected (400) or too large (413) - fails the same way every time.
func (e *TeamsError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
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
	timeout    time.Duration // how long one delivery may take; 0 = no own limit
}

// NewSimpleService creates a simpleService. Go has no constructors; a
// New... function plays that role.
//
// timeout limits one delivery as a whole, retries of the client included.
// When it runs out the request is cancelled, and Post returns an error for
// which errors.Is(err, context.DeadlineExceeded) is true.
func NewSimpleService(converter card.Converter, client *http.Client, webhookURL string, timeout time.Duration) Service {
	return simpleService{converter, client, webhookURL, timeout}
}

// Post renders the alert group into an Adaptive Card and sends it to Teams.
// On success it returns Teams' answer. A non-2xx answer is a *TeamsError.
func (s simpleService) Post(ctx context.Context, wm webhook.Message) ([]PostResponse, error) {
	ctx, span := trace.StartSpan(ctx, "simpleService.Post")
	defer span.End()

	c, err := s.converter.Convert(ctx, wm)
	if err != nil {
		// Two %w wrap both errors: the result "is" ErrCard and still
		// carries the cause, e.g. "failed to build the card: <cause>".
		return nil, fmt.Errorf("%w: %w", ErrCard, err)
	}

	pr, err := s.post(ctx, c)
	if err != nil {
		return nil, err
	}
	return []PostResponse{pr}, nil
}

// post sends the finished card bytes to Teams as they are.
//
// No error it returns contains the webhook URL: its query holds the
// signature, and errors end up in logs and in the answer to Alertmanager.
func (s simpleService) post(ctx context.Context, card json.RawMessage) (PostResponse, error) {
	ctx, span := trace.StartSpan(ctx, "simpleService.post")
	defer span.End()

	// The limit rides on the context: WithTimeout derives a context that is
	// cancelled when the time is up, and both the HTTP client and the retry
	// client stop as soon as the request's context is cancelled. cancel
	// stops the timer when the delivery finishes first; Go requires calling
	// it, hence the defer.
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(card))
	if err != nil {
		// The URL was validated at startup; this is not expected.
		return PostResponse{}, errors.New("failed to create the request to Teams")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		err = withoutURL(err)
		// Our own limit ran out (a cancel by the caller ends in
		// context.Canceled instead): say so in plain words.
		if s.timeout > 0 && errors.Is(err, context.DeadlineExceeded) {
			return PostResponse{}, fmt.Errorf("teams did not answer within %s: %w", s.timeout, err)
		}
		return PostResponse{}, fmt.Errorf("sending to Teams failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read at most maxResponseBody bytes; a read error only shortens the text.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	// Any 2xx is success; a Workflow answers 202 Accepted.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return PostResponse{}, &TeamsError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return PostResponse{Status: resp.StatusCode, Message: string(body)}, nil
}

// withoutURL removes the request URL from an HTTP client error. Go's client
// reports failures as *url.Error, whose text is `<op> "<full URL>": <cause>`,
// and wraps them again for each layer (here: the retry client inside the
// standard client); only the innermost cause is kept, e.g. "dial tcp ...:
// connection refused" or "context deadline exceeded".
func withoutURL(err error) error {
	// errors.As finds a *url.Error anywhere in the chain of wrapped errors.
	var ue *url.Error
	for errors.As(err, &ue) {
		err = ue.Err
	}
	return err
}
