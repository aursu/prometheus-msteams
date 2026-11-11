package card

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
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
		"note":      "line1\nline2 *x* [y](z) <b> 1+1",
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

// The default template escapes each value where it writes it: the card is
// valid, texts show the values as they are (markdown escaped), links keep
// them unchanged.
func Test_defaultTemplate_escapesValues(t *testing.T) {
	schema := compileCardSchema(t)
	tmpl, err := ParseTemplateFile(defaultTemplatePath)
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
	var card map[string]any
	if err := json.Unmarshal(cards[0], &card); err != nil {
		t.Fatal(err)
	}

	// Texts: the value, markdown escaped.
	texts := textsOf(card)
	for _, want := range []string{
		mdEscape(nastyLabels["alertname"]),
		mdEscape(nastyAnnotations["summary"]),
		mdEscape(nastyAnnotations["description"]),
	} {
		if !slices.Contains(texts, want) {
			t.Errorf("no text %q in %q", want, texts)
		}
	}
	facts := factsOf(card)
	for key, value := range map[string]string{
		"job":  nastyLabels["job"],
		"note": nastyLabels["note"],
	} {
		if got, want := facts[key], mdEscape(value); got != want {
			t.Errorf("fact %s = %q, want %q", key, got, want)
		}
	}
	if _, ok := facts[mdEscape("investigate_link")]; !ok {
		t.Errorf("no fact titled %q in %v", mdEscape("investigate_link"), facts)
	}

	// Links: the values unchanged.
	if got, want := actionURL(card, "Runbook"), nastyAnnotations["runbook"]; got != want {
		t.Errorf("Runbook URL = %q, want %q", got, want)
	}
	for title, labels := range map[string]template.KV{
		"Alertmanager": {"alertname": "high_memory_load"}, // the group
		"Silence":      nastyLabels,                       // the alert
	} {
		u := actionURL(card, title)
		if !strings.HasPrefix(u, "http://alertmanager.example.org:9093/#/") {
			t.Errorf("%s URL = %q, want one on the external URL", title, u)
		}
		_, filter, _ := strings.Cut(u, "filter=")
		// The filter is in the part after "#": a browser reads it back
		// with decodeURIComponent, for which "+" is a "+", not a space -
		// as url.PathUnescape, not url.QueryUnescape.
		if strings.Contains(filter, "+") {
			t.Errorf("%s filter = %q has a \"+\", which Alertmanager reads as part of a label name", title, filter)
		}
		if got, err := url.PathUnescape(filter); err != nil || got != labelMatchers(labels) {
			t.Errorf("%s filter = %q (%v), want %q", title, got, err, labelMatchers(labels))
		}
	}
}
