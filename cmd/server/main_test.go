package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Table-driven test (the usual Go style): each row is one case, the loop
// below runs every row as a named sub-test (shown as
// Test_validateWebhook/<name> in the output).
func Test_validateWebhook(t *testing.T) {
	// Made-up values; testSig is checked to never appear in an error text.
	const (
		testSig   = "Ogxlm1IT-Hs"
		testQuery = "?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=" + testSig
		// The URL shape Teams hands out today: explicit :443 and a
		// "/cu/<n>" segment. The upstream regular expression rejected it.
		currentURL = "https://default0123456789abcdef.ba.environment.api.powerplatform.com:443/powerautomate/automations/direct/cu/19/workflows/b008d545fb784/triggers/manual/paths/invoke" + testQuery
		// The same without the port and the "/cu/<n>" segment.
		shortURL = "https://example.cd.environment.api.powerplatform.com/powerautomate/automations/direct/workflows/b008d545fb784/triggers/manual/paths/invoke" + testQuery
		// A Workflow created before the move to Power Platform.
		logicAppsURL = "https://prod-12.westeurope.logic.azure.com:443/workflows/b008d545fb784/triggers/manual/paths/invoke" + testQuery
	)
	tests := []struct {
		name    string
		u       string
		wantErr bool
	}{
		{name: "current workflow url", u: currentURL},
		{name: "workflow url without port and cu segment", u: shortURL},
		{name: "logic apps workflow url", u: logicAppsURL},
		{name: "upper-case host", u: strings.Replace(shortURL, "example.cd.environment", "EXAMPLE.cd.ENVIRONMENT", 1)},

		{name: "no scheme", u: strings.TrimPrefix(shortURL, "https://"), wantErr: true},
		{name: "http", u: strings.Replace(shortURL, "https://", "http://", 1), wantErr: true},
		{name: "unrelated host", u: "https://example.com/workflows/x/triggers/manual/paths/invoke" + testQuery, wantErr: true},
		// The Workflow domain must be the end of the host, not just appear in it.
		{name: "workflow domain inside another host", u: strings.Replace(shortURL, ".environment.api.powerplatform.com", ".environment.api.powerplatform.com.example.org", 1), wantErr: true},
		{name: "path without workflows", u: strings.Replace(shortURL, "/workflows/", "/flows/", 1), wantErr: true},
		{name: "path with wrong ending", u: strings.Replace(shortURL, "/paths/invoke", "/paths/run", 1), wantErr: true},
		{name: "no sig", u: strings.Replace(shortURL, "&sig="+testSig, "", 1), wantErr: true},
		{name: "empty sig", u: strings.Replace(shortURL, "sig="+testSig, "sig=", 1), wantErr: true},
		{name: "no api-version", u: strings.Replace(shortURL, "api-version=1&", "", 1), wantErr: true},
		{name: "not a url", u: "https://exa mple.com/%zz", wantErr: true},
		// A retired Office 365 connector URL is no longer a valid target.
		{name: "office 365 connector", u: "https://example.webhook.office.com/webhookb2/5c51ab94-86c0-4ba3-a66c-c2ad73acc531@e3ebcd52-aa57-25e8-a214-94fb325450f4/IncomingWebhook/9f226e3c36fb47249f14d4dab2d5b845/92bac26e-62c5-427f-ac91-e51a268f94ca", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebhook(tt.u)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateWebhook() error = %v, wantErr %v", err, tt.wantErr)
			}
			// Errors go to logs, so they must not leak the signature.
			if err != nil && strings.Contains(err.Error(), testSig) {
				t.Errorf("validateWebhook() error leaks the signature: %v", err)
			}
		})
	}
}

// The client from newTeamsClient makes 1 + retryMax attempts on a 5xx, and
// in the end returns Teams' answer as it is (not an error), so the service
// can see the status code.
func Test_newTeamsClient_retries(t *testing.T) {
	tests := []struct {
		name      string
		retryMax  int
		wantCalls int32
	}{
		{name: "default: no retries, Alertmanager retries", retryMax: 0, wantCalls: 1},
		{name: "one retry", retryMax: 1, wantCalls: 2}, // waits about 1s between the two
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// The server runs its handler in another goroutine (Go's
			// lightweight thread); atomic.Int32 makes the count safe to
			// read from the test.
			var calls atomic.Int32
			teams := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer teams.Close()

			c := newTeamsClient(teamsClientOptions{retryMax: tt.retryMax})
			resp, err := c.Post(teams.URL, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("Post() error = %v, want the 503 answer", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", resp.StatusCode)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("Teams called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}

// A flag can come from the environment; the command line wins.
func Test_parseFlags(t *testing.T) {
	tests := []struct {
		name string
		env  string // value of TEAMS_TIMEOUT; "" = not set
		args []string
		want time.Duration
	}{
		{name: "default", want: 30 * time.Second},
		{name: "from the environment", env: "5s", want: 5 * time.Second},
		{name: "command line wins", env: "5s", args: []string{"-teams-timeout=7s"}, want: 7 * time.Second},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				// t.Setenv restores the variable when the test ends.
				t.Setenv("TEAMS_TIMEOUT", tt.env)
			}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			timeout := fs.Duration("teams-timeout", 30*time.Second, "")

			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			if *timeout != tt.want {
				t.Errorf("teams-timeout = %s, want %s", *timeout, tt.want)
			}
		})
	}
}

// The configuration file in the format the chart writes.
func Test_parseTeamsConfigFile(t *testing.T) {
	const config = `
connectors:
  - /alertmanager: https://example.org/one
connectors_with_custom_templates:
  - request_path: /custom
    template_file: /etc/template/card.tmpl
    webhook_url: https://example.org/two
    escape_underscores: true
`
	f := filepath.Join(t.TempDir(), "connectors.yaml")
	if err := os.WriteFile(f, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	tc, err := parseTeamsConfigFile(f)
	if err != nil {
		t.Fatal(err)
	}

	if len(tc.Connectors) != 1 || tc.Connectors[0]["/alertmanager"] != "https://example.org/one" {
		t.Errorf("Connectors = %v", tc.Connectors)
	}
	want := ConnectorWithCustomTemplate{
		RequestPath:       "/custom",
		TemplateFile:      "/etc/template/card.tmpl",
		WebhookURL:        "https://example.org/two",
		EscapeUnderscores: true,
	}
	if len(tc.ConnectorsWithCustomTemplates) != 1 || tc.ConnectorsWithCustomTemplates[0] != want {
		t.Errorf("ConnectorsWithCustomTemplates = %+v, want [%+v]", tc.ConnectorsWithCustomTemplates, want)
	}
}

// A broken file stops the program instead of starting with no routes.
func Test_parseTeamsConfigFile_invalid(t *testing.T) {
	f := filepath.Join(t.TempDir(), "connectors.yaml")
	if err := os.WriteFile(f, []byte("connectors: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamsConfigFile(f); err == nil {
		t.Error("parseTeamsConfigFile() succeeded on invalid YAML")
	}
}
