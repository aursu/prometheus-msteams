package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"testing"
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

func Test_extractWebhookFromRequest(t *testing.T) {
	oldStyleWebhook := "example.webhook.office.com/webhookb2/1e21eb6d-60f4-432f-9428-82cf86ec55a3@b12d4011-2ea0-4377-a99b-35c565546afd/IncomingWebhook/091d20cd2a594db0b2d7ed2937d7bd6d/91f7b1cc-96d0-4612-bedd-8b820c869464"
	newStyleWebhook := "example.webhook.office.com/webhookb2/5c51ab94-86c0-4ba3-a66c-c2ad73acc531@e3ebcd52-aa57-25e8-a214-94fb325450f4/IncomingWebhook/9f226e3c36fb47249f14d4dab2d5b845/92bac26e-62c5-427f-ac91-e51a268f94ca/V2QaKKUiGE6BMqWd-DeObKqCFmiQE5WSekPuAwjhc6ads1"
	workflowWebhook := "example.cd.environment.api.powerplatform.com/powerautomate/automations/direct/workflows/b008d545fb784/triggers/manual/paths/invoke?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=Ogxlm1IT-Hs"
	tests := []struct {
		name          string
		request       *http.Request
		webhookResult string
		wantErr       bool
	}{
		{
			name:          "standard oldstyle webhook",
			request:       newDummyRequest(fmt.Sprintf("/_dynamicwebhook/%s", oldStyleWebhook), ""),
			webhookResult: fmt.Sprintf("https://%s", oldStyleWebhook),
			wantErr:       false,
		},
		{
			name:          "standard newstyle webhook",
			request:       newDummyRequest(fmt.Sprintf("/_dynamicwebhook/%s", newStyleWebhook), ""),
			webhookResult: fmt.Sprintf("https://%s", newStyleWebhook),
			wantErr:       false,
		},
		{
			name:          "auth header oldstyle webhook",
			request:       newDummyRequest("/_dynamicwebhook/", fmt.Sprintf("webhook %s", oldStyleWebhook)),
			webhookResult: fmt.Sprintf("https://%s", oldStyleWebhook),
			wantErr:       false,
		},
		{
			name:          "auth header newstyle webhook",
			request:       newDummyRequest("/_dynamicwebhook/", fmt.Sprintf("webhook %s", newStyleWebhook)),
			webhookResult: fmt.Sprintf("https://%s", newStyleWebhook),
			wantErr:       false,
		},
		{
			name:    "invalid bearer",
			request: newDummyRequest("/_dynamicwebhook/", fmt.Sprintf("invalid-bearer %s", newStyleWebhook)),
			wantErr: true,
		},
		{
			name:    "missing webhook and header",
			request: newDummyRequest("/_dynamicwebhook/", ""),
			wantErr: true,
		},
		{
			name:          "workflow webhook",
			request:       newDummyRequest(fmt.Sprintf("/_dynamicwebhook/%s", workflowWebhook), ""),
			webhookResult: fmt.Sprintf("https://%s", workflowWebhook),
			wantErr:       false,
		},
	}
	prefix := "/_dynamicwebhook/"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			webhook, err := extractWebhookFromRequest(tt.request, prefix)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractWebhookFromRequest() error = %v, wantErr %v", err, tt.wantErr)
			} else if tt.webhookResult != webhook {
				t.Errorf("extractWebhookFromRequest() webhook (\"%s\") does not match expected webhook (\"%s\")", webhook, tt.webhookResult)
			}
		})
	}
}

func newDummyRequest(urlPath, authHeader string) *http.Request {
	url := fmt.Sprintf("http://localhost:2000%s", urlPath)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		panic(err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}
