package card

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
)

// Helpers that read a rendered card as generic JSON.

// walkCard calls fn for every JSON object in the card, depth first.
func walkCard(v any, fn func(map[string]any)) {
	switch node := v.(type) {
	case map[string]any:
		fn(node)
		for _, val := range node {
			walkCard(val, fn)
		}
	case []any:
		for _, val := range node {
			walkCard(val, fn)
		}
	}
}

// findElements returns every element of the given type.
func findElements(card map[string]any, typ string) []map[string]any {
	var found []map[string]any
	walkCard(card, func(m map[string]any) {
		if m["type"] == typ {
			found = append(found, m)
		}
	})
	return found
}

// findByID returns the element with the given id, or nil.
func findByID(card map[string]any, id string) map[string]any {
	var found map[string]any
	walkCard(card, func(m map[string]any) {
		if m["id"] == id {
			found = m
		}
	})
	return found
}

// textsOf returns the text of every TextBlock.
func textsOf(card map[string]any) []string {
	var texts []string
	for _, tb := range findElements(card, "TextBlock") {
		s, _ := tb["text"].(string)
		texts = append(texts, s)
	}
	return texts
}

// badgesOf returns the text of every Badge.
func badgesOf(card map[string]any) []string {
	var texts []string
	for _, b := range findElements(card, "Badge") {
		s, _ := b["text"].(string)
		texts = append(texts, s)
	}
	return texts
}

// factsOf returns every fact of every FactSet as title -> value.
func factsOf(card map[string]any) map[string]string {
	m := map[string]string{}
	for _, fs := range findElements(card, "FactSet") {
		list, _ := fs["facts"].([]any)
		for _, f := range list {
			fact, _ := f.(map[string]any)
			title, _ := fact["title"].(string)
			value, _ := fact["value"].(string)
			m[title] = value
		}
	}
	return m
}

// actionTitles returns the titles of the card's buttons, in order.
func actionTitles(card map[string]any) []string {
	var titles []string
	list, _ := card["actions"].([]any)
	for _, a := range list {
		action, _ := a.(map[string]any)
		title, _ := action["title"].(string)
		titles = append(titles, title)
	}
	return titles
}

// actionURL returns the URL of the button with the given title.
func actionURL(card map[string]any, title string) string {
	list, _ := card["actions"].([]any)
	for _, a := range list {
		action, _ := a.(map[string]any)
		if action["title"] == title {
			u, _ := action["url"].(string)
			return u
		}
	}
	return ""
}

// The scenarios below render alert groups built here, at a fixed time.

var cardNow = time.Date(2026, 11, 9, 20, 44, 0, 0, time.UTC)

// alertAt returns an alert that started minutesAgo before cardNow.
func alertAt(status string, minutesAgo int, labels, annotations template.KV) template.Alert {
	a := template.Alert{
		Status:       status,
		Labels:       labels,
		Annotations:  annotations,
		StartsAt:     cardNow.Add(-time.Duration(minutesAgo) * time.Minute),
		GeneratorURL: "https://prom.example.org/graph?g0.expr=up",
	}
	if status == "resolved" {
		a.EndsAt = cardNow
	}
	return a
}

// group builds the message Alertmanager sends for these alerts: the common
// labels and annotations are those all alerts share.
func group(status string, alerts ...template.Alert) webhook.Message {
	common := func(get func(template.Alert) template.KV) template.KV {
		kv := maps.Clone(get(alerts[0]))
		for _, a := range alerts[1:] {
			for k, v := range kv {
				if get(a)[k] != v {
					delete(kv, k)
				}
			}
		}
		return kv
	}
	return webhook.Message{
		Version: "4",
		Data: &template.Data{
			Receiver:          "teams",
			Status:            status,
			Alerts:            alerts,
			GroupLabels:       template.KV{"alertname": alerts[0].Labels["alertname"]},
			CommonLabels:      common(func(a template.Alert) template.KV { return a.Labels }),
			CommonAnnotations: common(func(a template.Alert) template.KV { return a.Annotations }),
			ExternalURL:       "https://alerts.example.org",
		},
	}
}

// renderDefault renders msg with the default template at cardNow, checks
// the card against the schema, and returns it as generic JSON.
func renderDefault(t *testing.T, msg webhook.Message) map[string]any {
	t.Helper()
	fixClock(t, cardNow)
	out, err := defaultCard(t).Convert(context.Background(), msg)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	cards := attachmentCards(t, out)
	if err := validateCard(compileCardSchema(t), cards[0]); err != nil {
		t.Fatalf("card does not match the schema: %v\n%s", err, cards[0])
	}
	var card map[string]any
	if err := json.Unmarshal(cards[0], &card); err != nil {
		t.Fatal(err)
	}
	return card
}

func endpointLabels(name, instance string) template.KV {
	return template.KV{"alertname": "EndpointDown", "severity": "critical", "cluster": "office",
		"job": "blackbox", "name": name, "instance": instance}
}

var troubleshooting = template.KV{
	"summary":     "the site fails",
	"access":      "- Grafana: https://grafana.example.org/d/sites",
	"check":       "curl -sS -o /dev/null -w '%{http_code}\\n' https://www.example.org/\ndig +short www.example.org",
	"mitigate":    "1. Confirm from outside.\n2. Roll back the last change.",
	"runbook_url": "https://runbooks.example.org/endpoint-down",
}

// One firing alert: heading, badges, time, facts, both sections, all four
// buttons.
func Test_defaultCard_firingAlert(t *testing.T) {
	card := renderDefault(t, group("firing",
		alertAt("firing", 10, endpointLabels("www", "https://www.example.org/"), troubleshooting)))

	texts := textsOf(card)
	for _, want := range []string{
		"EndpointDown",
		"the site fails",
		"Started {{TIME(2026-11-09T20:34:00Z)}} · firing for 10 min",
	} {
		if !slices.Contains(texts, want) {
			t.Errorf("no text %q in %q", want, texts)
		}
	}
	if got, want := badgesOf(card), []string{"FIRING", "critical", "office"}; !slices.Equal(got, want) {
		t.Errorf("badges = %q, want %q", got, want)
	}
	icon := findElements(card, "Icon")[0]
	if icon["name"] != "ErrorCircle" || icon["color"] != "Attention" {
		t.Errorf("icon = %v, want ErrorCircle in Attention", icon)
	}
	// Facts: the labels worth reading, not alertname, severity, cluster.
	facts := findElements(card, "FactSet")[0]["facts"].([]any)
	var titles []string
	for _, f := range facts {
		titles = append(titles, f.(map[string]any)["title"].(string))
	}
	if want := []string{"instance", "job", "name"}; !slices.Equal(titles, want) {
		t.Errorf("fact titles = %q, want %q", titles, want)
	}

	// Troubleshooting: hidden, toggled by its heading, with the three steps.
	body := findByID(card, "troubleshooting-body")
	if body == nil || body["isVisible"] != false {
		t.Fatalf("troubleshooting body = %v, want a hidden element", body)
	}
	code := findElements(body, "CodeBlock")
	if len(code) != 1 || code[0]["codeSnippet"] != troubleshooting["check"] || code[0]["language"] != "Bash" {
		t.Errorf("check = %v, want the check annotation as Bash", code)
	}
	bodyTexts := textsOf(body)
	for _, want := range []string{troubleshooting["access"], troubleshooting["mitigate"], "[Full runbook](https://runbooks.example.org/endpoint-down)"} {
		if !slices.Contains(bodyTexts, want) {
			t.Errorf("troubleshooting has no %q in %q", want, bodyTexts)
		}
	}
	if !slices.Contains(texts, "access · check · mitigate") {
		t.Errorf("no hint naming the three steps in %q", texts)
	}
	if findByID(card, "details-body") == nil {
		t.Error("no \"All labels and annotations\" section")
	}

	if got, want := actionTitles(card), []string{"Alertmanager", "Silence", "Runbook", "Graph"}; !slices.Equal(got, want) {
		t.Errorf("buttons = %q, want %q", got, want)
	}
	if got := actionURL(card, "Graph"); got != "https://prom.example.org/graph?g0.expr=up" {
		t.Errorf("Graph URL = %q", got)
	}
}

// A resolved alert: green, how long it lasted, no sections, only Graph.
func Test_defaultCard_resolvedAlert(t *testing.T) {
	card := renderDefault(t, group("resolved",
		alertAt("resolved", 13, endpointLabels("www", "https://www.example.org/"), troubleshooting)))

	want := "Lasted 13 min · {{TIME(2026-11-09T20:31:00Z)}} – {{TIME(2026-11-09T20:44:00Z)}}"
	if !slices.Contains(textsOf(card), want) {
		t.Errorf("no text %q in %q", want, textsOf(card))
	}
	if got := badgesOf(card); got[0] != "RESOLVED" {
		t.Errorf("badges = %q, want RESOLVED first", got)
	}
	if findByID(card, "troubleshooting-body") != nil || findByID(card, "details-body") != nil {
		t.Error("a resolved card has collapsed sections")
	}
	if got, want := actionTitles(card), []string{"Graph"}; !slices.Equal(got, want) {
		t.Errorf("buttons = %q, want %q", got, want)
	}
}

// An alert that started more than 12 hours ago shows the date too.
func Test_defaultCard_oldAlertShowsDate(t *testing.T) {
	card := renderDefault(t, group("firing",
		alertAt("firing", 26*60, endpointLabels("www", "https://www.example.org/"), nil)))
	want := "Started {{DATE(2026-11-08T18:44:00Z, SHORT)}} {{TIME(2026-11-08T18:44:00Z)}} · firing for 1 d 2 h"
	if !slices.Contains(textsOf(card), want) {
		t.Errorf("no text %q in %q", want, textsOf(card))
	}
}

// A group whose alerts share the summary: the summary once, a table keyed
// by the label that differs, the alert count in the title and a badge.
func Test_defaultCard_groupSharedSummary(t *testing.T) {
	ann := template.KV{"summary": "Pod has been in a non-ready state for more than 15 minutes."}
	labels := func(pod string) template.KV {
		return template.KV{"alertname": "KubePodNotReady", "severity": "warning", "namespace": "web", "pod": pod}
	}
	card := renderDefault(t, group("firing",
		alertAt("firing", 30, labels("web-1"), ann),
		alertAt("firing", 20, labels("web-2"), ann),
		alertAt("firing", 10, labels("web-3"), ann)))

	texts := textsOf(card)
	if !slices.Contains(texts, "KubePodNotReady ×3") {
		t.Errorf("no title with the count in %q", texts)
	}
	if !slices.Contains(badgesOf(card), "3 alerts") {
		t.Errorf("no count badge in %q", badgesOf(card))
	}
	if n := strings.Count(strings.Join(texts, "\n"), ann["summary"]); n != 1 {
		t.Errorf("summary shown %d times, want once", n)
	}
	// The earliest start of the group.
	if !slices.Contains(texts, "Started {{TIME(2026-11-09T20:14:00Z)}} · firing for 30 min") {
		t.Errorf("no start of the earliest alert in %q", texts)
	}
	if got := factsOf(card)["namespace"]; got != "web" {
		t.Errorf("fact namespace = %q, want the shared label", got)
	}

	table := findElements(card, "Table")
	if len(table) != 1 {
		t.Fatalf("want one table, got %d", len(table))
	}
	tableTexts := textsOf(table[0])
	want := []string{"pod", "since",
		"web-1", "{{TIME(2026-11-09T20:14:00Z)}}",
		"web-2", "{{TIME(2026-11-09T20:24:00Z)}}",
		"web-3", "{{TIME(2026-11-09T20:34:00Z)}}"}
	if !slices.Equal(tableTexts, want) {
		t.Errorf("table = %q, want %q", tableTexts, want)
	}
	if got, want := actionTitles(card), []string{"Alertmanager", "Silence", "Graph"}; !slices.Equal(got, want) {
		t.Errorf("buttons = %q, want %q", got, want)
	}
}

// A group whose alerts have their own summaries: no shared summary line,
// each row shows its alert's; troubleshooting is the first alert's, and the
// card says so.
func Test_defaultCard_groupOwnSummaries(t *testing.T) {
	alert := func(name string, minutes int) template.Alert {
		ann := maps.Clone(troubleshooting)
		ann["summary"] = fmt.Sprintf("%s fails", name)
		ann["check"] = fmt.Sprintf("curl -sS https://%s/", name)
		return alertAt("firing", minutes, endpointLabels(name, "https://"+name+"/"), ann)
	}
	card := renderDefault(t, group("firing", alert("a.example.org", 5), alert("b.example.org", 6)))

	texts := textsOf(card)
	table := textsOf(findElements(card, "Table")[0])
	for _, want := range []string{"https://a.example.org/", "a.example.org fails", "https://b.example.org/", "b.example.org fails"} {
		if !slices.Contains(table, want) {
			t.Errorf("table has no %q: %q", want, table)
		}
	}
	// The summaries are only in the table, not above it as well.
	if n := strings.Count(strings.Join(texts, "\n"), "a.example.org fails"); n != 1 {
		t.Errorf("an alert's summary is shown %d times, want once (in the table): %q", n, texts)
	}
	body := findByID(card, "troubleshooting-body")
	if got := findElements(body, "CodeBlock")[0]["codeSnippet"]; got != "curl -sS https://a.example.org/" {
		t.Errorf("check = %q, want the first alert's", got)
	}
	if !slices.Contains(textsOf(body), "For the first alert of the group: https://a.example.org/") {
		t.Errorf("no note naming the first alert in %q", textsOf(body))
	}
}

// A group with alerts left out: the count includes them, and a line links
// to the whole group.
func Test_defaultCard_truncatedGroup(t *testing.T) {
	msg := group("firing",
		alertAt("firing", 5, endpointLabels("a", "https://a/"), nil),
		alertAt("firing", 5, endpointLabels("b", "https://b/"), nil))
	msg.TruncatedAlerts = 8
	card := renderDefault(t, msg)
	texts := strings.Join(textsOf(card), "\n")
	if !strings.Contains(texts, "EndpointDown ×10") {
		t.Errorf("title does not count the left-out alerts: %q", texts)
	}
	if !strings.Contains(texts, "8 more alerts not shown. [Open the group in Alertmanager](https://alerts.example.org/#/alerts?") {
		t.Errorf("no line about the left-out alerts: %q", texts)
	}
}

// Without an external URL there is nowhere to send the reader: no
// Alertmanager and no Silence button.
func Test_defaultCard_noExternalURL(t *testing.T) {
	msg := group("firing", alertAt("firing", 5, endpointLabels("www", "https://www/"), nil))
	msg.ExternalURL = ""
	card := renderDefault(t, msg)
	if got, want := actionTitles(card), []string{"Graph"}; !slices.Equal(got, want) {
		t.Errorf("buttons = %q, want %q", got, want)
	}
}

// Without troubleshooting annotations there is no Troubleshooting section;
// a warning alert gets the warning look.
func Test_defaultCard_noTroubleshooting(t *testing.T) {
	labels := template.KV{"alertname": "TargetDown", "severity": "warning", "job": "node-exporter"}
	card := renderDefault(t, group("firing", alertAt("firing", 5, labels, template.KV{"summary": "down"})))
	if findByID(card, "troubleshooting-body") != nil {
		t.Error("Troubleshooting section without troubleshooting annotations")
	}
	if icon := findElements(card, "Icon")[0]; icon["name"] != "Warning" || icon["color"] != "Warning" {
		t.Errorf("icon = %v, want Warning in Warning", icon)
	}
}
