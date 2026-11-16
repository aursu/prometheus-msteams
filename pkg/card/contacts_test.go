package card

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
)

// testContacts is a contacts file with two groups and a default.
const testContacts = `
default: platform
groups:
  platform:
    primary: [{name: Jane Doe, id: jane@example.org}]
    backup:  [{name: John Roe, id: john@example.org}]
  shop:
    primary: [{name: Ann Lee, id: ann@example.org}, {name: John Roe, id: john@example.org}]
    backup:  [{name: Jane Doe, id: jane@example.org}]
  storage:
    primary: [{name: Kim Park, id: kim@example.org, mention: false, note: Storage - contact personally}]
    responders: platform
`

// writeContacts writes body to a contacts file and returns its path.
func writeContacts(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contacts.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadTestContacts loads testContacts.
func loadTestContacts(t *testing.T) *Contacts {
	t.Helper()
	c, err := LoadContacts(writeContacts(t, testContacts))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadContacts_defaults(t *testing.T) {
	c := loadTestContacts(t)
	if c.Label != "owner" {
		t.Errorf("Label = %q, want owner", c.Label)
	}
	if !slices.Equal(c.MentionSeverities, []string{"critical"}) {
		t.Errorf("MentionSeverities = %v, want [critical]", c.MentionSeverities)
	}
}

func TestLoadContacts_neverMention(t *testing.T) {
	c, err := LoadContacts(writeContacts(t, testContacts+"mention_severities: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MentionSeverities) != 0 {
		t.Errorf("MentionSeverities = %v, want none", c.MentionSeverities)
	}
}

func TestLoadContacts_errors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"unknown key", "groupz: {}\n", "groupz"},
		{"no groups", "label: team\n", "no groups"},
		{"empty group", "groups: {a: {}}\n", `group "a" has nobody`},
		{"no id", "groups: {a: {primary: [{name: A}]}}\n", "name and an id"},
		{"no name", "groups: {a: {backup: [{id: a@example.org}]}}\n", "name and an id"},
		{"unknown default", "default: b\ngroups: {a: {primary: [{name: A, id: a}]}}\n", `default "b"`},
		{"unknown responders", "groups: {a: {primary: [{name: A, id: a}], responders: b}}\n", `responders "b" is not`},
		{"responders of responders", "groups: {a: {primary: [{name: A, id: a}], responders: b}, b: {primary: [{name: B, id: b}], responders: a}}\n", "of its own"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadContacts(writeContacts(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
	if _, err := LoadContacts(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file: no error")
	}
}

// names returns the names of people.
func names(people []Person) []string {
	var n []string
	for _, p := range people {
		n = append(n, p.Name)
	}
	return n
}

// contactAlert returns an alert with the given status, severity and owner
// ("" for none).
func contactAlert(status, severity, owner string) template.Alert {
	labels := template.KV{"alertname": "A", "severity": severity}
	if owner != "" {
		labels["owner"] = owner
	}
	return template.Alert{Status: status, Labels: labels}
}

func TestContacts_For(t *testing.T) {
	c := loadTestContacts(t)
	tests := []struct {
		name                        string
		alerts                      template.Alerts
		mention                     bool
		primary, backup, responders []string
	}{
		{"firing critical: mentioned", template.Alerts{contactAlert("firing", "critical", "platform")},
			true, []string{"Jane Doe"}, []string{"John Roe"}, nil},
		{"firing warning: named only", template.Alerts{contactAlert("firing", "warning", "platform")},
			false, []string{"Jane Doe"}, []string{"John Roe"}, nil},
		{"resolved: named only", template.Alerts{contactAlert("resolved", "critical", "platform")},
			false, []string{"Jane Doe"}, []string{"John Roe"}, nil},
		{"no label: the default group", template.Alerts{contactAlert("firing", "critical", "")},
			true, []string{"Jane Doe"}, []string{"John Roe"}, nil},
		{"unknown value: the default group", template.Alerts{contactAlert("firing", "critical", "nobody")},
			true, []string{"Jane Doe"}, []string{"John Roe"}, nil},
		{"two groups: each person once, primary wins",
			template.Alerts{contactAlert("firing", "critical", "shop"), contactAlert("firing", "warning", "platform")},
			true, []string{"Ann Lee", "John Roe", "Jane Doe"}, nil, nil},
		{"only the firing alerts count",
			template.Alerts{contactAlert("resolved", "critical", "shop"), contactAlert("firing", "warning", "platform")},
			false, []string{"Jane Doe"}, []string{"John Roe"}, nil},
		{"responders: named after the group's people", template.Alerts{contactAlert("firing", "critical", "storage")},
			true, []string{"Kim Park"}, nil, []string{"Jane Doe", "John Roe"}},
		{"responders who are named already: once", template.Alerts{contactAlert("firing", "critical", "storage"), contactAlert("firing", "critical", "platform")},
			true, []string{"Kim Park", "Jane Doe"}, []string{"John Roe"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.For(tt.alerts)
			if got == nil {
				t.Fatal("For = nil")
			}
			if got.Label != "owner" {
				t.Errorf("Label = %q, want owner", got.Label)
			}
			if got.Mention != tt.mention {
				t.Errorf("Mention = %v, want %v", got.Mention, tt.mention)
			}
			if !slices.Equal(names(got.Primary), tt.primary) {
				t.Errorf("Primary = %v, want %v", names(got.Primary), tt.primary)
			}
			if !slices.Equal(names(got.Backup), tt.backup) {
				t.Errorf("Backup = %v, want %v", names(got.Backup), tt.backup)
			}
			if !slices.Equal(names(got.Responders), tt.responders) {
				t.Errorf("Responders = %v, want %v", names(got.Responders), tt.responders)
			}
		})
	}
}

func TestContacts_For_nobody(t *testing.T) {
	alerts := template.Alerts{contactAlert("firing", "critical", "platform")}
	var none *Contacts
	if got := none.For(alerts); got != nil {
		t.Errorf("no contacts file: For = %+v, want nil", got)
	}
	c, err := LoadContacts(writeContacts(t, "groups: {shop: {primary: [{name: A, id: a}]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.For(alerts); got != nil {
		t.Errorf("no group and no default: For = %+v, want nil", got)
	}
}

// renderWithContacts renders msg with the default template and the given
// contacts at cardNow, checks it against the schema, and returns it.
func renderWithContacts(t *testing.T, c *Contacts, msg webhook.Message) map[string]any {
	t.Helper()
	fixClock(t, cardNow)
	tmpl, err := ParseTemplateFile(defaultTemplatePath, c)
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewTemplatedCardCreator(tmpl, false).Convert(context.Background(), msg)
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

// entitiesOf returns the card's msteams.entities as "text -> id".
func entitiesOf(card map[string]any) map[string]string {
	m := map[string]string{}
	msteams, _ := card["msteams"].(map[string]any)
	list, _ := msteams["entities"].([]any)
	for _, e := range list {
		entity, _ := e.(map[string]any)
		mentioned, _ := entity["mentioned"].(map[string]any)
		if entity["type"] == "mention" {
			m[entity["text"].(string)], _ = mentioned["id"].(string)
		}
	}
	return m
}

func Test_defaultCard_contacts(t *testing.T) {
	c := loadTestContacts(t)
	labels := func(severity string) template.KV {
		return template.KV{"alertname": "EndpointDown", "severity": severity, "owner": "platform", "zone": "a", "job": "blackbox"}
	}
	tests := []struct {
		name     string
		contacts *Contacts
		msg      webhook.Message
		line     string
		entities map[string]string
	}{
		{"firing critical: @mentions", c,
			group("firing", alertAt("firing", 5, labels("critical"), template.KV{"summary": "down"})),
			"contacts: <at>Jane Doe</at> (primary), <at>John Roe</at> (backup)",
			map[string]string{"<at>Jane Doe</at>": "jane@example.org", "<at>John Roe</at>": "john@example.org"}},
		{"firing warning: names", c,
			group("firing", alertAt("firing", 5, labels("warning"), template.KV{"summary": "down"})),
			"contacts: Jane Doe (primary), John Roe (backup)", map[string]string{}},
		{"resolved: names", c,
			group("resolved", alertAt("resolved", 5, labels("critical"), template.KV{"summary": "down"})),
			"contacts: Jane Doe (primary), John Roe (backup)", map[string]string{}},
		{"responders: @mentioned, a person with mention false only named", c,
			group("firing", alertAt("firing", 5, template.KV{"alertname": "EndpointDown", "severity": "critical", "owner": "storage", "zone": "a", "job": "blackbox"}, template.KV{"summary": "down"})),
			"contacts: Kim Park (primary, Storage - contact personally) · responders: <at>Jane Doe</at>, <at>John Roe</at>",
			map[string]string{"<at>Jane Doe</at>": "jane@example.org", "<at>John Roe</at>": "john@example.org"}},
		{"no contacts file: no line", nil,
			group("firing", alertAt("firing", 5, labels("critical"), template.KV{"summary": "down"})),
			"", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := renderWithContacts(t, tt.contacts, tt.msg)
			var line string
			for _, text := range textsOf(card) {
				if strings.HasPrefix(text, "contacts: ") {
					line = text
				}
			}
			if line != tt.line {
				t.Errorf("contacts line = %q, want %q", line, tt.line)
			}
			if got := entitiesOf(card); !maps.Equal(got, tt.entities) {
				t.Errorf("entities = %v, want %v", got, tt.entities)
			}
			// With a contacts file the owner label is the last fact, right above
			// the contacts line; without one it is an ordinary fact.
			wantFacts := []string{"job", "zone", "owner"}
			if tt.contacts == nil {
				wantFacts = []string{"job", "owner", "zone"}
			}
			if got := summaryFactTitles(card); !slices.Equal(got, wantFacts) {
				t.Errorf("facts = %v, want %v", got, wantFacts)
			}
		})
	}
}
