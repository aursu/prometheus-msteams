package main

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aursu/prometheus-msteams/pkg/card"
	"github.com/aursu/prometheus-msteams/pkg/testutils"
	"github.com/aursu/prometheus-msteams/pkg/transport"
)

const (
	defaultTemplateFile = "../../default-message-workflow-card.tmpl"
	routesTestSig       = "SECRET-SIG"
	// A URL that passes -validate-webhook-url; nothing is sent to it.
	validWorkflowURL = "https://example.cd.environment.api.powerplatform.com/powerautomate/automations/direct/workflows/abc/triggers/manual/paths/invoke?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=" + routesTestSig
)

// newTestBuilder returns a routeBuilder with the default template, URL
// validation on and logs thrown away.
func newTestBuilder(t *testing.T) routeBuilder {
	t.Helper()
	tmpl, err := card.ParseTemplateFile(defaultTemplateFile)
	if err != nil {
		t.Fatal(err)
	}
	return routeBuilder{
		logger:              slog.New(slog.DiscardHandler),
		client:              http.DefaultClient,
		validateURLs:        true,
		defaultTemplate:     tmpl,
		defaultTemplateFile: defaultTemplateFile,
	}
}

// paths returns the request paths of routes, sorted.
func paths(routes []transport.Route) []string {
	var p []string
	for _, r := range routes {
		p = append(p, r.RequestPath)
	}
	slices.Sort(p)
	return p
}

// recordingTeams plays Teams and records the path of each request, so a
// test can see which URL a route sent to.
func recordingTeams(t *testing.T) (baseURL string, got func() []string) {
	t.Helper()
	var mu sync.Mutex // the handler runs in the server's goroutines
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// post sends the sample alert group through a route.
func post(t *testing.T, r transport.Route) {
	t.Helper()
	msg, err := testutils.ParseWebhookJSONFromFile("../../pkg/card/testdata/prom_post_request.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Service.Post(context.Background(), msg); err != nil {
		t.Fatalf("route %s: Post() error = %v", r.RequestPath, err)
	}
}

func Test_routeBuilder_build(t *testing.T) {
	tc := PromTeamsConfig{
		Connectors: []map[string]string{
			{"/a": validWorkflowURL, "/b": validWorkflowURL},
			{"/c": validWorkflowURL},
		},
		ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/custom", WebhookURL: validWorkflowURL, TemplateFile: defaultTemplateFile},
		},
	}
	routes, err := newTestBuilder(t).build(tc)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(routes), []string{"/a", "/b", "/c", "/custom"}; !slices.Equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

// Each route sends to its own URL.
func Test_routeBuilder_build_sendsToItsURL(t *testing.T) {
	teams, got := recordingTeams(t)
	b := newTestBuilder(t)
	b.validateURLs = false // the fake Teams has a plain http URL

	routes, err := b.build(PromTeamsConfig{
		Connectors: []map[string]string{{"/a": teams + "/hook-a"}},
		ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/custom", WebhookURL: teams + "/hook-custom", TemplateFile: defaultTemplateFile},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		post(t, r)
	}
	if g, want := got(), []string{"/hook-a", "/hook-custom"}; !slices.Equal(g, want) {
		t.Errorf("Teams got requests for %v, want %v", g, want)
	}
}

// A configuration error stops the program at startup; the error names the
// problem but never contains a webhook URL.
func Test_routeBuilder_build_errors(t *testing.T) {
	badTemplate := filepath.Join(t.TempDir(), "bad.tmpl")
	if err := os.WriteFile(badTemplate, []byte(`{{ define "teams.card" }}{{ end`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidURL := "http://example.org/hook?sig=" + routesTestSig
	emptyFile := writeFile(t, "empty", " \n")
	invalidURLFile := writeFile(t, "invalid", invalidURL+"\n")

	tests := []struct {
		name    string
		tc      PromTeamsConfig
		wantErr string
	}{
		{name: "invalid URL", tc: PromTeamsConfig{Connectors: []map[string]string{{"/a": invalidURL}}}, wantErr: "request path /a:"},
		{name: "invalid URL, custom template", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURL: invalidURL, TemplateFile: defaultTemplateFile}}}, wantErr: "request path /c:"},
		{name: "no request_path", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{WebhookURL: validWorkflowURL, TemplateFile: defaultTemplateFile}}}, wantErr: "no request_path"},
		{name: "no webhook URL", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", TemplateFile: defaultTemplateFile}}}, wantErr: "request path /c: webhook_url or webhook_url_file is required"},
		{name: "webhook_url and webhook_url_file", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURL: validWorkflowURL, WebhookURLFile: invalidURLFile}}}, wantErr: "not both"},
		{name: "URL file missing", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURLFile: "/nonexistent/url"}}}, wantErr: "/nonexistent/url"},
		{name: "URL file empty", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURLFile: emptyFile}}}, wantErr: "is empty"},
		{name: "invalid URL in file", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURLFile: invalidURLFile}}}, wantErr: "request path /c:"},
		{name: "template file missing", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURL: validWorkflowURL, TemplateFile: "/nonexistent.tmpl"}}}, wantErr: "/nonexistent.tmpl"},
		{name: "template does not parse", tc: PromTeamsConfig{ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/c", WebhookURL: validWorkflowURL, TemplateFile: badTemplate}}}, wantErr: "request path /c:"},
		{name: "duplicate request path", tc: PromTeamsConfig{
			Connectors: []map[string]string{{"/a": validWorkflowURL}},
			ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
				{RequestPath: "/a", WebhookURL: validWorkflowURL, TemplateFile: defaultTemplateFile}}}, wantErr: "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTestBuilder(t).build(tt.tc)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("build() error = %v, want one containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), routesTestSig) {
				t.Errorf("error leaks the signature: %v", err)
			}
		})
	}
}

// writeFile stores content in a temporary file and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The URL can come from a file (a mounted Secret): the line break at its
// end is dropped. Without template_file the default template is used.
func Test_routeBuilder_build_urlFile(t *testing.T) {
	teams, got := recordingTeams(t)
	b := newTestBuilder(t)
	b.validateURLs = false

	routes, err := b.build(PromTeamsConfig{
		ConnectorsWithCustomTemplates: []ConnectorWithCustomTemplate{
			{RequestPath: "/from-file", WebhookURLFile: writeFile(t, "url", teams+"/hook-file\n")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	post(t, routes[0])
	if g, want := got(), []string{"/hook-file"}; !slices.Equal(g, want) {
		t.Errorf("Teams got requests for %v, want %v", g, want)
	}
}

// The flags give one connector: a request path with a URL, inline or from a
// file.
func Test_flagConnector(t *testing.T) {
	urlFile := writeFile(t, "url", validWorkflowURL+"\n")
	tests := []struct {
		name                     string
		requestURI, inline, file string
		want                     map[string]string
		wantErr                  string
	}{
		{name: "no flags"},
		{name: "path without URL", requestURI: "/a"},
		{name: "URL without path", inline: validWorkflowURL},
		{name: "inline URL", requestURI: "/a", inline: validWorkflowURL, want: map[string]string{"/a": validWorkflowURL}},
		{name: "URL from file", requestURI: "/a", file: urlFile, want: map[string]string{"/a": validWorkflowURL}},
		{name: "both", requestURI: "/a", inline: validWorkflowURL, file: urlFile, wantErr: "not both"},
		{name: "file missing", requestURI: "/a", file: "/nonexistent/url", wantErr: "/nonexistent/url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := flagConnector(tt.requestURI, tt.inline, tt.file)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("flagConnector() error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// maps.Equal compares two maps key by key; two nil maps are equal.
			if !maps.Equal(got, tt.want) {
				t.Errorf("flagConnector() = %v, want %v", got, tt.want)
			}
		})
	}
}

// With -validate-webhook-url=false any URL is taken.
func Test_routeBuilder_build_noValidation(t *testing.T) {
	b := newTestBuilder(t)
	b.validateURLs = false
	if _, err := b.build(PromTeamsConfig{Connectors: []map[string]string{{"/a": "http://example.org/hook"}}}); err != nil {
		t.Errorf("build() error = %v, want none", err)
	}
}
