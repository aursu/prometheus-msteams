package card

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
)

// Values that break a card when written into it without escaping: quotes,
// a backslash, a line break, markdown characters, HTML, underscores in
// links.
var (
	nastyLabels = template.KV{
		"alertname": "high_memory_load",
		"severity":  "critical",
		"job":       `node "exporter"`,
		"note":      "line1\nline2 *x* [y](z) <b>",
	}
	nastyAnnotations = template.KV{
		"summary":          `disk "C:\" 95%_full`,
		"description":      "see *details*",
		"runbook":          "https://wiki.example.org/runbooks/high_memory_load?x=a_b&y=1",
		"investigate_link": " https://example.org/q?a_b=1 ",
	}
)

func nastyMessage() webhook.Message {
	return webhook.Message{
		Version:  "4",
		GroupKey: "{}:{alertname=\"high_memory_load\"}",
		Data: &template.Data{
			Receiver:          "teams",
			Status:            "firing",
			Alerts:            template.Alerts{{Status: "firing", Labels: nastyLabels, Annotations: nastyAnnotations}},
			GroupLabels:       template.KV{"alertname": "high_memory_load"},
			CommonLabels:      nastyLabels,
			CommonAnnotations: nastyAnnotations,
			ExternalURL:       "http://alertmanager.example.org:9093",
		},
	}
}

// renderedCard holds the parts of a card the checks below read. (Field
// tags name the JSON keys.)
type renderedCard struct {
	Body []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Facts []struct {
			Title string `json:"title"`
			Value string `json:"value"`
		} `json:"facts"`
	} `json:"body"`
	Actions []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"actions"`
}

// facts returns the card's facts as title -> value.
func (c renderedCard) facts() map[string]string {
	m := map[string]string{}
	for _, b := range c.Body {
		for _, f := range b.Facts {
			m[f.Title] = f.Value
		}
	}
	return m
}

// actionURL returns the URL of the button with the given title.
func (c renderedCard) actionURL(title string) string {
	for _, a := range c.Actions {
		if a.Title == title {
			return a.URL
		}
	}
	return ""
}

// The templates escape each value where they write it: the card is valid,
// texts show the values as they are (markdown escaped), links keep them
// unchanged.
func Test_templates_escapeValues(t *testing.T) {
	schema := compileCardSchema(t)
	templates := []string{
		"../../default-message-workflow-card.tmpl",
		"../../examples/templates/default-message-workflow-card-actions.tmpl",
	}
	for _, file := range templates {
		t.Run(file, func(t *testing.T) {
			tmpl, err := ParseTemplateFile(file)
			if err != nil {
				t.Fatal(err)
			}
			out, err := NewTemplatedCardCreator(tmpl, false).Convert(context.Background(), nastyMessage())
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			cards := attachmentCards(t, out)
			if err := validateCard(schema, cards[0]); err != nil {
				t.Fatalf("card does not match the schema: %v", err)
			}
			var card renderedCard
			if err := json.Unmarshal(cards[0], &card); err != nil {
				t.Fatal(err)
			}

			// Texts: the value, markdown escaped.
			if got, want := card.Body[1].Text, mdEscape(nastyAnnotations["summary"]); got != want {
				t.Errorf("subtitle = %q, want %q", got, want)
			}
			facts := card.facts()
			for key, value := range map[string]string{
				"job":     nastyLabels["job"],
				"note":    nastyLabels["note"],
				"summary": nastyAnnotations["summary"],
			} {
				if got, want := facts[key], mdEscape(value); got != want {
					t.Errorf("fact %s = %q, want %q", key, got, want)
				}
			}
			if _, ok := facts[mdEscape("investigate_link")]; !ok {
				t.Errorf("no fact titled %q in %v", mdEscape("investigate_link"), facts)
			}

			if !strings.Contains(file, "actions") {
				return
			}
			// Links: the values unchanged.
			if got, want := card.actionURL("runbook"), nastyAnnotations["runbook"]; got != want {
				t.Errorf("runbook URL = %q, want %q", got, want)
			}
			if got, want := card.actionURL("Investigate"), strings.TrimSpace(nastyAnnotations["investigate_link"]); got != want {
				t.Errorf("Investigate URL = %q, want %q", got, want)
			}
			am := card.actionURL("View in Alertmanager")
			if !strings.HasPrefix(am, "http://alertmanager.example.org:9093/#/alerts?") {
				t.Errorf("Alertmanager URL = %q, want the alert list of the external URL", am)
			}
			_, filter, _ := strings.Cut(am, "filter=")
			if got, err := url.QueryUnescape(filter); err != nil || got != labelMatchers(nastyLabels) {
				t.Errorf("Alertmanager filter = %q (%v), want %q", got, err, labelMatchers(nastyLabels))
			}
		})
	}
}
