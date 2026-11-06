package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
// entry.
func (b routeBuilder) customRoute(c ConnectorWithCustomTemplate) (transport.Route, error) {
	if c.RequestPath == "" {
		return transport.Route{}, errors.New("one of the connectors_with_custom_templates has no request_path")
	}
	if c.WebhookURL == "" {
		return transport.Route{}, fmt.Errorf("request path %s: webhook_url is required", c.RequestPath)
	}
	if err := b.checkURL(c.RequestPath, c.WebhookURL); err != nil {
		return transport.Route{}, err
	}
	if c.TemplateFile == "" {
		return transport.Route{}, fmt.Errorf("request path %s: template_file is required", c.RequestPath)
	}
	tmpl, err := card.ParseTemplateFile(c.TemplateFile)
	if err != nil {
		return transport.Route{}, fmt.Errorf("request path %s: %w", c.RequestPath, err)
	}
	return b.route(c.RequestPath, c.WebhookURL, b.converter(tmpl, c.TemplateFile, c.EscapeUnderscores)), nil
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

// converter renders cards with tmpl and logs them at debug level.
func (b routeBuilder) converter(tmpl *template.Template, file string, escapeUnderscores bool) card.Converter {
	return card.NewCreatorLoggingMiddleware(
		b.logger.With("template_file", file, "escaped_underscores", escapeUnderscores),
		card.NewTemplatedCardCreator(tmpl, escapeUnderscores),
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
