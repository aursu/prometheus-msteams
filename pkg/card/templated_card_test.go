package card

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aursu/prometheus-msteams/pkg/testutils"
	"github.com/google/go-cmp/cmp"
)

const (
	testTextBlockType = "TextBlock"
	testPromAlertFile = "./testdata/prom_post_request.json"
)

// writeTemplate stores a "teams.card" template with the given body in a
// temporary file and returns its path. t.TempDir() is removed automatically
// when the test ends.
func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "card.tmpl")
	content := `{{ define "teams.card" }}` + body + `{{ end }}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// convert renders templateFile with the sample alert group.
func convert(t *testing.T, templateFile string) (json.RawMessage, error) {
	t.Helper()
	tmpl, err := ParseTemplateFile(templateFile)
	if err != nil {
		t.Fatal(err)
	}
	a, err := testutils.ParseWebhookJSONFromFile(testPromAlertFile)
	if err != nil {
		t.Fatal(err)
	}
	return NewTemplatedCardCreator(tmpl, false).Convert(context.Background(), a)
}

// firstCard decodes the result and returns the first attachment's card as a
// generic JSON object (map from key to any value), for the checks below.
func firstCard(t *testing.T, got json.RawMessage) map[string]interface{} {
	t.Helper()
	var msg struct {
		Attachments []struct {
			Content map[string]interface{} `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(msg.Attachments))
	}
	return msg.Attachments[0].Content
}

// bodyOf returns the "body" list of a card.
func bodyOf(t *testing.T, card map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := card["body"].([]interface{})
	if !ok {
		t.Fatalf("card has no body list, got: %v", card)
	}
	body := make([]map[string]interface{}, 0, len(raw))
	for _, e := range raw {
		elem, ok := e.(map[string]interface{})
		if !ok {
			t.Fatalf("body element is not an object: %T", e)
		}
		body = append(body, elem)
	}
	return body
}

func Test_templatedCard_Convert(t *testing.T) {
	tests := []struct {
		name         string
		templateFile string
		// checkFn asserts what matters for the case without hard-coding the
		// whole card.
		checkFn func(t *testing.T, got json.RawMessage)
	}{
		{
			// The default template, one firing alert: heading, summary,
			// description, time, facts, and the collapsed labels section.
			name:         "default template layout",
			templateFile: "../../default-message-workflow-card.tmpl",
			checkFn: func(t *testing.T, got json.RawMessage) {
				t.Helper()
				body := bodyOf(t, firstCard(t, got))
				var types []string
				for _, elem := range body {
					if typ, ok := elem["type"].(string); ok {
						types = append(types, typ)
					}
				}
				wantTypes := []string{"ColumnSet", testTextBlockType, testTextBlockType, testTextBlockType, "FactSet", "Container"}
				if diff := cmp.Diff(wantTypes, types); diff != "" {
					t.Fatalf("body element types mismatch (-want +got):\n%s", diff)
				}
				facts, ok := body[4]["facts"].([]interface{})
				if !ok || len(facts) == 0 {
					t.Fatalf("FactSet element missing facts, got: %v", body[4])
				}
			},
		},
		{
			// ColumnSet with nested columns and items must come out whole.
			name:         "custom template keeps ColumnSet",
			templateFile: "./testdata/workflow_columnset.tmpl",
			checkFn: func(t *testing.T, got json.RawMessage) {
				t.Helper()
				body := bodyOf(t, firstCard(t, got))
				if len(body) != 1 {
					t.Fatalf("want 1 body element, got %d", len(body))
				}
				elem := body[0]
				if typ, _ := elem["type"].(string); typ != "ColumnSet" {
					t.Fatalf("want body element type %q, got %q", "ColumnSet", typ)
				}
				if style, _ := elem["style"].(string); style != "attention" {
					t.Fatalf("want ColumnSet style %q, got %q", "attention", style)
				}
				columns, ok := elem["columns"].([]interface{})
				if !ok || len(columns) == 0 {
					t.Fatalf("ColumnSet missing columns, got: %v", elem)
				}
				col, ok := columns[0].(map[string]interface{})
				if !ok {
					t.Fatalf("column is not an object, got: %T", columns[0])
				}
				items, ok := col["items"].([]interface{})
				if !ok || len(items) == 0 {
					t.Fatalf("Column missing items, got: %v", col)
				}
			},
		},
		{
			// Card-level fields the former Go struct did not know were dropped,
			// and backgroundImage / contentUrl were added. Now the card is
			// passed through exactly.
			name: "card-level fields pass through, nothing is added",
			templateFile: writeTemplate(t, `{
				"type": "message",
				"attachments": [{
					"contentType": "application/vnd.microsoft.card.adaptive",
					"content": {
						"type": "AdaptiveCard",
						"version": "1.5",
						"fallbackText": "fallback",
						"selectAction": {"type": "Action.OpenUrl", "url": "https://example.org"},
						"body": [{"type": "TextBlock", "text": "x"}]
					}
				}]
			}`),
			checkFn: func(t *testing.T, got json.RawMessage) {
				t.Helper()
				card := firstCard(t, got)
				if card["fallbackText"] != "fallback" {
					t.Errorf("fallbackText lost, card: %v", card)
				}
				if _, ok := card["selectAction"]; !ok {
					t.Errorf("selectAction lost, card: %v", card)
				}
				for _, added := range []string{"backgroundImage", "msteams"} {
					if _, ok := card[added]; ok {
						t.Errorf("%s was added, card: %v", added, card)
					}
				}
				if strings.Contains(string(got), "contentUrl") {
					t.Errorf("contentUrl was added: %s", got)
				}
			},
		},
		{
			// The template's whitespace and line breaks are removed.
			name:         "result is compact JSON",
			templateFile: "../../default-message-workflow-card.tmpl",
			checkFn: func(t *testing.T, got json.RawMessage) {
				t.Helper()
				var compact bytes.Buffer
				if err := json.Compact(&compact, got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(compact.Bytes(), got) {
					t.Errorf("result is not compact:\n%s", got)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, err := convert(t, tt.templateFile)
			if err != nil {
				t.Fatalf("templatedCard.Convert() error = %v", err)
			}
			tt.checkFn(t, got)
		})
	}
}

// Template output that a Workflow would not accept must fail in Convert,
// before anything is sent.
func Test_templatedCard_Convert_rejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string // a part of the expected error text
	}{
		{
			name:    "invalid JSON",
			body:    `{"type": "message", "attachments": [}`,
			wantErr: "not valid JSON",
		},
		{
			name:    "wrong type",
			body:    `{"type": "MessageCard", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive", "content": {}}]}`,
			wantErr: `type "MessageCard"`,
		},
		{
			name:    "no attachments",
			body:    `{"type": "message", "attachments": []}`,
			wantErr: "no attachments",
		},
		{
			name:    "attachment is not an Adaptive Card",
			body:    `{"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.hero", "content": {}}]}`,
			wantErr: `contentType "application/vnd.microsoft.card.hero"`,
		},
		{
			name:    "attachment without content",
			body:    `{"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive"}]}`,
			wantErr: "no content",
		},
		{
			name:    "attachment with null content",
			body:    `{"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive", "content": null}]}`,
			wantErr: "no content",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, err := convert(t, writeTemplate(t, tt.body))
			if err == nil {
				t.Fatalf("templatedCard.Convert() = %s, want an error", got)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}
