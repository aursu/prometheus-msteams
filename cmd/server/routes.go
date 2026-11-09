package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/alertmanager/template"

	"github.com/aursu/prometheus-msteams/pkg/card"
	"github.com/aursu/prometheus-msteams/pkg/service"
	"github.com/aursu/prometheus-msteams/pkg/transport"
)

// routeBuilder turns the configuration into the served routes: one route
// per request path, each with its Teams Workflow URL and its card template.
// Its fields are what every route shares; build does the work.
type routeBuilder struct {
	logger       *slog.Logger
	client       *http.Client
	teamsTimeout time.Duration
	validateURLs bool // -validate-webhook-url
	maxCardSize  int  // -max-card-size

	// The template given by -template-file, used by "connectors".
	defaultTemplate     *template.Template
	defaultTemplateFile string
	escapeUnderscores   bool // -auto-escape-underscores, for "connectors"
}

// build returns the routes for tc, or the first configuration error. The
// errors name the request path and, where it matters, a file, but never a
// webhook URL: its query holds the signature.
func (b routeBuilder) build(tc PromTeamsConfig) ([]transport.Route, error) {
	var routes []transport.Route

	// "connectors": request path -> URL, all with the default template.
	defaultConverter := b.converter(b.defaultTemplate, b.defaultTemplateFile, b.escapeUnderscores)
	for _, c := range tc.Connectors {
		for path, webhookURL := range c {
			if err := b.checkURL(path, webhookURL); err != nil {
				return nil, err
			}
			routes = append(routes, b.route(path, webhookURL, defaultConverter))
		}
	}

	// "connectors_with_custom_templates": one entry per request path.
	for _, c := range tc.ConnectorsWithCustomTemplates {
		r, err := b.customRoute(c)
		if err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}

	if err := checkDuplicateRequestPath(routes); err != nil {
		return nil, err
	}
	return routes, nil
}

// customRoute builds the route of one "connectors_with_custom_templates"
// entry. The URL is given inline (webhook_url) or in a file
// (webhook_url_file); without template_file the default template is used.
func (b routeBuilder) customRoute(c ConnectorWithCustomTemplate) (transport.Route, error) {
	if c.RequestPath == "" {
		return transport.Route{}, errors.New("one of the connectors_with_custom_templates has no request_path")
	}
	webhookURL, err := webhookURLFrom(c.WebhookURL, c.WebhookURLFile)
	if err != nil {
		return transport.Route{}, fmt.Errorf("request path %s: %w", c.RequestPath, err)
	}
	if err := b.checkURL(c.RequestPath, webhookURL); err != nil {
		return transport.Route{}, err
	}

	tmpl, file := b.defaultTemplate, b.defaultTemplateFile
	if c.TemplateFile != "" {
		tmpl, err = card.ParseTemplateFile(c.TemplateFile)
		if err != nil {
			return transport.Route{}, fmt.Errorf("request path %s: %w", c.RequestPath, err)
		}
		file = c.TemplateFile
	}
	return b.route(c.RequestPath, webhookURL, b.converter(tmpl, file, c.EscapeUnderscores)), nil
}

// webhookURLFrom returns the URL given inline or, if file is set, read from
// that file. Exactly one of the two must be set.
func webhookURLFrom(inline, file string) (string, error) {
	switch {
	case inline != "" && file != "":
		return "", errors.New("set webhook_url or webhook_url_file, not both")
	case file != "":
		return readURLFile(file)
	case inline != "":
		return inline, nil
	default:
		return "", errors.New("webhook_url or webhook_url_file is required")
	}
}

// readURLFile reads a webhook URL from a file, such as a key of a
// Kubernetes Secret mounted into the pod. Spaces and line breaks around the
// URL are dropped: "echo URL > file" and many Secrets end with a line break.
// The file is read once, at startup; a new URL needs a restart.
//
// Errors name the file, never its content.
func readURLFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path comes from the configuration
	if err != nil {
		// The error from os.ReadFile names the path and the reason
		// ("no such file or directory", "permission denied").
		return "", fmt.Errorf("cannot read the webhook URL file: %w", err)
	}
	u := strings.TrimSpace(string(b))
	if u == "" {
		return "", fmt.Errorf("the webhook URL file %s is empty", path)
	}
	return u, nil
}

// flagConnector returns the connector given by the flags: -teams-request-uri
// with -teams-incoming-webhook-url or -teams-incoming-webhook-url-file. nil
// when the flags give none.
func flagConnector(requestURI, inline, file string) (map[string]string, error) {
	if requestURI == "" || (inline == "" && file == "") {
		return nil, nil
	}
	webhookURL, err := webhookURLFrom(inline, file)
	if err != nil {
		return nil, fmt.Errorf("-teams-incoming-webhook-url / -teams-incoming-webhook-url-file: %w", err)
	}
	return map[string]string{requestURI: webhookURL}, nil
}

// checkURL applies -validate-webhook-url to one URL.
func (b routeBuilder) checkURL(path, webhookURL string) error {
	if !b.validateURLs {
		return nil
	}
	if err := validateWebhook(webhookURL); err != nil {
		return fmt.Errorf("request path %s: %w", path, err)
	}
	return nil
}

// converter renders cards with tmpl, keeps them within -max-card-size and
// logs them at debug level. Each layer wraps the next one.
func (b routeBuilder) converter(tmpl *template.Template, file string, escapeUnderscores bool) card.Converter {
	logger := b.logger.With("template_file", file, "escaped_underscores", escapeUnderscores)
	return card.NewCreatorLoggingMiddleware(logger,
		card.NewSizeLimiter(logger,
			card.NewTemplatedCardCreator(tmpl, escapeUnderscores),
			b.maxCardSize,
		),
	)
}

// route connects a request path to a Teams Workflow URL.
func (b routeBuilder) route(path, webhookURL string, converter card.Converter) transport.Route {
	s := service.NewSimpleService(converter, b.client, webhookURL, b.teamsTimeout)
	return transport.Route{RequestPath: path, Service: service.NewLoggingService(b.logger, s)}
}

func checkDuplicateRequestPath(routes []transport.Route) error {
	added := map[string]bool{}
	for _, r := range routes {
		if _, ok := added[r.RequestPath]; ok {
			return fmt.Errorf("found duplicate use of request path '%s'", r.RequestPath)
		}
		added[r.RequestPath] = true
	}
	return nil
}
