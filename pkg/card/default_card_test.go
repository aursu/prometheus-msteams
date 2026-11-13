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

// summaryFactTitles returns the titles of the facts under the summary - a
// FactSet directly in the body, not the one inside "All labels and
// annotations" - in order.
func summaryFactTitles(card map[string]any) []string {
	var titles []string
	body, _ := card["body"].([]any)
	for _, el := range body {
		if fs, _ := el.(map[string]any); fs["type"] == "FactSet" {
			for _, f := range fs["facts"].([]any) {
				titles = append(titles, f.(map[string]any)["title"].(string))
			}
		}
	}
	return titles
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
	return template.KV{"alertname": "EndpointDown", "severity": "critical", "cluster": "prod",
		"job": "blackbox", "name": name, "instance": instance}
}

var troubleshooting = template.KV{
	"summary":     "the site fails",
	"access":      "- Grafana: https://grafana.example.org/d/sites",
	"check":       "curl -sS -o /dev/null -w '%{http_code}\\n' https://www.example.org/\ndig +short www.example.org",
	"mitigate":    "1. Confirm from outside.\n2. Roll back the last change.",
	"runbook_url": "https://runbooks.example.org/endpoint-down",
}

// One firing alert: heading, where, badges, time, facts, both sections, all
// four buttons.
func Test_defaultCard_firingAlert(t *testing.T) {
	card := renderDefault(t, group("firing",
		alertAt("firing", 10, endpointLabels("www", "https://www.example.org/"), troubleshooting)))

	texts := textsOf(card)
	for _, want := range []string{
		"Endpoint Down",           // the title: the alert name as words
		"alertname: EndpointDown", // and the name itself, under the time line
		"Cluster: prod",           // where: only the cluster is known
		"the site fails",
		"Started {{TIME(2026-11-09T20:34:00Z)}} · firing for 10 min",
	} {
		if !slices.Contains(texts, want) {
			t.Errorf("no text %q in %q", want, texts)
		}
	}
	if got, want := badgesOf(card), []string{"FIRING", "critical"}; !slices.Equal(got, want) {
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
	// The hints sit in columns that a narrow card (a phone) leaves out.
	for _, col := range findElements(card, "Column") {
		if slices.Contains(textsOf(col), "access · check · mitigate") && col["targetWidth"] != "atLeast:Standard" {
			t.Errorf("hint column targetWidth = %v, want atLeast:Standard", col["targetWidth"])
		}
	}
	if findByID(card, "details-body") == nil {
		t.Error("no \"All labels and annotations\" section")
	}
	// One alert: nothing to list.
	if findByID(card, "alerts-body") != nil || len(findElements(card, "Table")) != 0 {
		t.Error("a single alert has an alerts section")
	}

	if got, want := actionTitles(card), []string{"Alertmanager", "Silence", "Runbook", "Graph"}; !slices.Equal(got, want) {
		t.Errorf("buttons = %q, want %q", got, want)
	}
	if got := actionURL(card, "Graph"); got != "https://prom.example.org/graph?g0.expr=up" {
		t.Errorf("Graph URL = %q", got)
	}
}

// The "where" line: general to specific, from the labels the alert has.
func Test_defaultCard_where(t *testing.T) {
	tests := []struct {
		name      string
		labels    template.KV
		where     string
		facts     []string // fact titles, in order
		noneFacts []string // labels that must not be facts
	}{
		{
			name: "Kubernetes object from its label",
			labels: template.KV{"alertname": "KubeDeploymentReplicasMismatch", "severity": "warning",
				"env": "prod", "cluster": "prod-1", "namespace": "web", "deployment": "api", "job": "kube-state-metrics"},
			where:     "Env: prod · Cluster: prod-1 · Namespace/web · Deployment/api",
			facts:     []string{"job"},
			noneFacts: []string{"env", "cluster", "namespace", "deployment"},
		},
		{
			name: "every part; the object label wins over the Kubernetes labels",
			labels: template.KV{"alertname": "EndpointDown", "severity": "critical", "datacenter": "eu-1",
				"project": "shop", "env": "prod", "cluster": "prod-1", "namespace": "web",
				"object": "Service/www", "pod": "www-1"},
			where: "IDC: eu-1 · Project: shop · Env: prod · Cluster: prod-1 · Namespace/web · Service/www",
			// pod is not the object here, so it stays a fact.
			facts:     []string{"pod"},
			noneFacts: []string{"datacenter", "project", "env", "cluster", "namespace", "object"},
		},
		{
			name: "not Kubernetes: environment wins over env",
			labels: template.KV{"alertname": "EndpointDown", "severity": "critical", "datacenter": "eu-1",
				"project": "shop", "environment": "stage", "env": "other", "host": "web1.example.org", "name": "shop"},
			where:     "IDC: eu-1 · Project: shop · Env: stage · Host: web1.example.org",
			facts:     []string{"name"},
			noneFacts: []string{"datacenter", "project", "environment", "host"},
		},
		{
			name:   "nothing known",
			labels: template.KV{"alertname": "Watchdog", "severity": "none"},
			where:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := renderDefault(t, group("firing", alertAt("firing", 5, tt.labels, template.KV{"summary": "s"})))
			body := card["body"].([]any)
			// The first element is the heading: the icon column, then the
			// column with the title, the where line and the badges.
			heading := body[0].(map[string]any)["columns"].([]any)[1].(map[string]any)
			// Under the title: the where line (a TextBlock), or straight away
			// the badges (a ColumnSet) when nothing is known.
			second := heading["items"].([]any)[1].(map[string]any)
			if tt.where == "" {
				if second["type"] != "ColumnSet" {
					t.Errorf("under the title: %v, want the badges", second)
				}
			} else if second["type"] != "TextBlock" || second["text"] != tt.where {
				t.Errorf("under the title: %v, want the where line %q", second, tt.where)
			}
			titles := summaryFactTitles(card)
			if tt.facts != nil && !slices.Equal(titles, tt.facts) {
				t.Errorf("fact titles = %q, want %q", titles, tt.facts)
			}
			for _, l := range tt.noneFacts {
				if slices.Contains(titles, l) {
					t.Errorf("%s is a fact as well as in the where line", l)
				}
			}
			if fb, _ := card["fallbackText"].(string); tt.where != "" && !strings.Contains(fb, tt.where) {
				t.Errorf("fallbackText %q does not say where", fb)
			}
		})
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
	if !slices.Contains(texts, "Kube Pod Not Ready ×3") {
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
	// The shared namespace is the "where" line, not a fact; the pods differ,
	// so there is no object.
	if !slices.Contains(texts, "Namespace/web") {
		t.Errorf("no where line with the shared namespace in %q", texts)
	}
	if slices.Contains(summaryFactTitles(card), "namespace") {
		t.Error("namespace is shown as a fact as well as in the where line")
	}

	// The alerts: a table in a collapsed section titled by the label that
	// tells them apart and their count.
	if !slices.Contains(texts, "Affected pods (3)") {
		t.Errorf("no section \"Affected pods (3)\" in %q", texts)
	}
	section := findByID(card, "alerts-body")
	if section == nil || section["isVisible"] != false {
		t.Fatalf("alerts section body = %v, want a hidden element", section)
	}
	table := findElements(section, "Table")
	if len(table) != 1 || len(findElements(card, "Table")) != 1 {
		t.Fatalf("want one table, in the alerts section; got %d there", len(table))
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

// The alerts section of a group: kept on a resolved card, titled "Alerts"
// when no row label tells the alerts apart, and without a hint column - its
// count is in the title, which a phone shows whole.
func Test_defaultCard_alertsSection(t *testing.T) {
	labels := func(reason string) template.KV {
		return template.KV{"alertname": "KubeJobFailed", "severity": "warning", "namespace": "web", "reason": reason}
	}
	tests := []struct {
		name, status, title string
	}{
		{"firing, no row label", "firing", "Alerts (2)"},
		{"resolved keeps the list", "resolved", "Alerts (2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := renderDefault(t, group(tt.status,
				alertAt(tt.status, 5, labels("BackoffLimitExceeded"), nil),
				alertAt(tt.status, 6, labels("DeadlineExceeded"), nil)))
			if findByID(card, "alerts-body") == nil {
				t.Fatal("no alerts section")
			}
			// The section's heading line: arrow, icon, title - no hint.
			var heading map[string]any
			for _, cs := range findElements(card, "ColumnSet") {
				if sa, _ := cs["selectAction"].(map[string]any); sa != nil && sa["title"] == tt.title {
					heading = cs
				}
			}
			if heading == nil {
				t.Fatalf("no section titled %q in %q", tt.title, textsOf(card))
			}
			if n := len(heading["columns"].([]any)); n != 3 {
				t.Errorf("heading has %d columns, want 3 (no hint column)", n)
			}
			if got := len(textsOf(findByID(card, "alerts-body"))); got != 2*3 {
				t.Errorf("table has %d texts, want a header and two rows of two", got)
			}
		})
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
	if !strings.Contains(texts, "Endpoint Down ×10") {
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

// The dashboard_url and alert_url annotations: the Dashboard and Alert rule
// buttons, first, also on a resolved card. A group gets only the links its
// alerts share; a dashboard they do not share is linked from each row.
func Test_defaultCard_links(t *testing.T) {
	const ruleURL = "https://grafana.example.org/alerting/list?search=EndpointDown"
	dashboard := func(name string) string { return "https://grafana.example.org/d/site?var-target=" + name }
	alert := func(status, name string, ann template.KV) template.Alert {
		ann = maps.Clone(ann)
		ann["summary"] = "the site fails"
		return alertAt(status, 5, endpointLabels(name, "https://"+name+"/"), ann)
	}
	tests := []struct {
		name    string
		msg     webhook.Message
		buttons []string
		urls    map[string]string
		rows    []string // the table's first-column texts, for a group
	}{
		{
			name: "one alert",
			msg: group("firing", alert("firing", "www.example.org",
				template.KV{"dashboard_url": " " + dashboard("www") + "\n", "alert_url": ruleURL, "runbook_url": "https://runbooks.example.org/x"})),
			buttons: []string{"Dashboard", "Alert rule", "Alertmanager", "Silence", "Runbook", "Graph"},
			urls:    map[string]string{"Dashboard": dashboard("www"), "Alert rule": ruleURL},
		},
		{
			name: "resolved keeps the links",
			msg: group("resolved", alert("resolved", "www.example.org",
				template.KV{"dashboard_url": dashboard("www"), "alert_url": ruleURL})),
			buttons: []string{"Dashboard", "Alert rule", "Graph"},
			urls:    map[string]string{"Dashboard": dashboard("www"), "Alert rule": ruleURL},
		},
		{
			name: "group, own dashboards: linked from the rows",
			msg: group("firing",
				alert("firing", "a.example.org", template.KV{"dashboard_url": dashboard("a"), "alert_url": ruleURL}),
				alert("firing", "b.example.org", template.KV{"dashboard_url": dashboard("b"), "alert_url": ruleURL})),
			buttons: []string{"Alert rule", "Alertmanager", "Silence", "Graph"},
			urls:    map[string]string{"Alert rule": ruleURL},
			rows: []string{
				"[https://a.example.org/](" + dashboard("a") + ")",
				"[https://b.example.org/](" + dashboard("b") + ")"},
		},
		{
			name: "group, shared dashboard: the button, plain rows",
			msg: group("firing",
				alert("firing", "a.example.org", template.KV{"dashboard_url": dashboard("all")}),
				alert("firing", "b.example.org", template.KV{"dashboard_url": dashboard("all")})),
			buttons: []string{"Dashboard", "Alertmanager", "Silence", "Graph"},
			urls:    map[string]string{"Dashboard": dashboard("all")},
			rows:    []string{"https://a.example.org/", "https://b.example.org/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := renderDefault(t, tt.msg)
			if got := actionTitles(card); !slices.Equal(got, tt.buttons) {
				t.Errorf("buttons = %q, want %q", got, tt.buttons)
			}
			for title, want := range tt.urls {
				if got := actionURL(card, title); got != want {
					t.Errorf("%s URL = %q, want %q", title, got, want)
				}
			}
			if tt.rows != nil {
				table := textsOf(findElements(card, "Table")[0])
				for _, want := range tt.rows {
					if !slices.Contains(table, want) {
						t.Errorf("table has no row %q: %q", want, table)
					}
				}
			}
			// Shown as buttons, so not repeated under "All labels and annotations".
			if details := findByID(card, "details-body"); details != nil {
				for title := range factsOf(details) {
					if strings.Contains(title, "url") {
						t.Errorf("%q is listed under all labels and annotations", title)
					}
				}
			}
		})
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

// The title: the rule's title annotation, else the alert name as words; the
// alert name itself under the time line when it reads differently.
func Test_defaultCard_title(t *testing.T) {
	labels := func(alertname, name string) template.KV {
		return template.KV{"alertname": alertname, "severity": "warning", "cluster": "prod", "name": name}
	}
	tests := []struct {
		name      string
		msg       webhook.Message
		title     string
		alertname string // shown under the time line; "" = not shown
	}{
		{
			name:      "no title annotation: the alert name as words",
			msg:       group("firing", alertAt("firing", 5, labels("KubeAPIErrorBudgetBurn", "a"), template.KV{"summary": "s"})),
			title:     "Kube API Error Budget Burn",
			alertname: "KubeAPIErrorBudgetBurn",
		},
		{
			name: "title annotation, trimmed",
			msg: group("firing", alertAt("firing", 5, labels("KubeAPIErrorBudgetBurn", "a"),
				template.KV{"summary": "s", "title": " Kubernetes API is *slow*\n"})),
			title:     `Kubernetes API is \*slow\*`,
			alertname: "KubeAPIErrorBudgetBurn",
		},
		{
			name:  "one word: no second name",
			msg:   group("firing", alertAt("firing", 5, labels("Watchdog", "a"), template.KV{"summary": "s"})),
			title: "Watchdog",
		},
		{
			name: "group, shared title",
			msg: group("firing",
				alertAt("firing", 5, labels("EndpointDown", "a"), template.KV{"title": "Site is not reachable"}),
				alertAt("firing", 5, labels("EndpointDown", "b"), template.KV{"title": "Site is not reachable"})),
			title:     "Site is not reachable ×2",
			alertname: "EndpointDown",
		},
		{
			name: "group, titles differ: the alert name as words",
			msg: group("firing",
				alertAt("firing", 5, labels("EndpointDown", "a"), template.KV{"title": "Site a is down"}),
				alertAt("firing", 5, labels("EndpointDown", "b"), template.KV{"title": "Site b is down"})),
			title:     "Endpoint Down ×2",
			alertname: "EndpointDown",
		},
		{
			name: "resolved",
			msg: group("resolved", alertAt("resolved", 5, labels("NodeFilesystemSpaceFillingUp", "a"),
				template.KV{"title": "Disk is filling up"})),
			title:     "Disk is filling up",
			alertname: "NodeFilesystemSpaceFillingUp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := renderDefault(t, tt.msg)
			heading := card["body"].([]any)[0].(map[string]any)["columns"].([]any)[1].(map[string]any)
			items := heading["items"].([]any)
			if got := items[0].(map[string]any)["text"]; got != tt.title {
				t.Errorf("title = %q, want %q", got, tt.title)
			}
			// The badges row holds badges only.
			badgeRow := items[len(items)-1].(map[string]any)
			for _, col := range badgeRow["columns"].([]any) {
				if len(findElements(col.(map[string]any), "Badge")) == 0 {
					t.Errorf("badges row has a column without a badge: %v", col)
				}
			}
			// Under the time line: "alertname: <name>", or nothing more.
			body := card["body"].([]any)
			var under string
			for i, el := range body {
				text, _ := el.(map[string]any)["text"].(string)
				if (strings.HasPrefix(text, "Started ") || strings.HasPrefix(text, "Lasted ")) && i+1 < len(body) {
					under, _ = body[i+1].(map[string]any)["text"].(string)
				}
			}
			named := strings.HasPrefix(under, "alertname: ")
			if tt.alertname == "" && named {
				t.Errorf("under the time line: %q, want no alert name", under)
			}
			if tt.alertname != "" && under != "alertname: "+tt.alertname {
				t.Errorf("under the time line: %q, want %q", under, "alertname: "+tt.alertname)
			}
			// fallbackText is plain text: the title without the count and without
			// markdown escapes.
			plain := strings.ReplaceAll(strings.TrimSuffix(tt.title, " ×2"), `\`, "")
			if fb, _ := card["fallbackText"].(string); !strings.HasPrefix(fb, "[FIRING] "+plain) && !strings.HasPrefix(fb, "[RESOLVED] "+plain) {
				t.Errorf("fallbackText %q does not start with the title", fb)
			}
			if _, ok := factsOf(card)["title"]; ok {
				t.Error("the title annotation is repeated under All labels and annotations")
			}
		})
	}
}
