package card

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Masterminds/sprig/v3"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
	"go.yaml.in/yaml/v3"
)

// What a Teams Workflow accepts: a message whose attachments are Adaptive
// Cards.
const (
	workflowCardType   = "message"
	adaptiveCardFormat = "application/vnd.microsoft.card.adaptive"
)

// envelope is the outer part of a Workflow message, the only part this
// program reads:
//
//	{"type": "message", "attachments": [{"contentType": "...", "content": {...card...}}]}
//
// The card itself (content) stays json.RawMessage: it is checked to exist,
// never parsed or rewritten. (The `json:"..."` tags name the JSON key each
// field is read from.)
type envelope struct {
	Type        string `json:"type"`
	Attachments []struct {
		ContentType string          `json:"contentType"`
		Content     json.RawMessage `json:"content"`
	} `json:"attachments"`
}

// templatedCard implements Converter using Alert manager templating.
type templatedCard struct {
	template *template.Template
	// If true, replace all character `_` with `\\_` in the prometheus alert.
	escapeUnderscores bool
}

// NewTemplatedCardCreator creates a templatedCard.
func NewTemplatedCardCreator(template *template.Template, escapeUnderscores bool) Converter {
	return &templatedCard{template, escapeUnderscores}
}

// Convert renders the template "teams.card" with the alert group, checks
// that the result is a message Teams Workflows accept (see checkEnvelope) and
// returns it compacted. Nothing is added to or removed from what the template
// wrote. A bad template is an error here, before anything is sent to Teams.
// (The Converter interface passes a context; rendering does not use it, so
// the parameter is named _, Go's name for "not used".)
func (m *templatedCard) Convert(_ context.Context, promAlert webhook.Message) (json.RawMessage, error) {
	cardString, err := m.executeTemplate(promAlert)
	if err != nil {
		return nil, err
	}

	if err := checkEnvelope([]byte(cardString)); err != nil {
		return nil, err
	}

	// json.Compact copies the JSON without the whitespace and line breaks of
	// the template. Same content, far fewer bytes (Teams limits the size).
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(cardString)); err != nil {
		return nil, fmt.Errorf("template output is not valid JSON: %w", err)
	}
	return compact.Bytes(), nil
}

// checkEnvelope checks only what a Workflow needs to accept the message at
// all: valid JSON, type "message", at least one attachment, every attachment
// an Adaptive Card with content. The card content itself is not validated
// here; the tests validate our templates against the Adaptive Card schema.
func checkEnvelope(b []byte) error {
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return fmt.Errorf("template output is not valid JSON: %w", err)
	}
	if e.Type != workflowCardType {
		return fmt.Errorf("template output has type %q, want %q", e.Type, workflowCardType)
	}
	if len(e.Attachments) == 0 {
		return errors.New("template output has no attachments")
	}
	// range gives index and element; i is used in the messages.
	for i, a := range e.Attachments {
		if a.ContentType != adaptiveCardFormat {
			return fmt.Errorf("attachment %d has contentType %q, want %q", i, a.ContentType, adaptiveCardFormat)
		}
		if len(a.Content) == 0 || string(a.Content) == "null" {
			return fmt.Errorf("attachment %d has no content", i)
		}
	}
	return nil
}

func (m *templatedCard) executeTemplate(promAlert webhook.Message) (string, error) {
	// TODO(bzon): Maybe we can escape underscores after the office 365 card is finally created?
	// That approach would be simpler to read and probably a performance gain because
	// we don't have to run json.NewEncoder(v).Encode() multiple times.
	if m.escapeUnderscores {
		promAlert = jsonEscapeMessage(promAlert)
	}

	data := &template.Data{
		Receiver:          promAlert.Receiver,
		Status:            promAlert.Status,
		Alerts:            promAlert.Alerts,
		GroupLabels:       promAlert.GroupLabels,
		CommonLabels:      promAlert.CommonLabels,
		CommonAnnotations: promAlert.CommonAnnotations,
		ExternalURL:       promAlert.ExternalURL,
	}

	cardString, err := m.template.ExecuteTextString(
		`{{ template "teams.card" . }}`, data,
	)
	if err != nil {
		return "", fmt.Errorf("failed to template alerts: %w", err)
	}

	return cardString, nil
}

func jsonEncode(str string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(str)
	if err != nil {
		return ""
	}
	return string(buf.Bytes()[1 : len(buf.Bytes())-2])
}

// json escape all string values in kvData and also escape
// '_' char so it does not get processed as markdown italic
func jsonEncodeAlertmanagerKV(kvData template.KV) {
	for k, v := range kvData {
		kvData[k] = strings.ReplaceAll(jsonEncode(v), `_`, `\\_`)
	}
}

func jsonEscapeMessage(promAlert webhook.Message) webhook.Message {
	retPromAlert := promAlert
	jsonEncodeAlertmanagerKV(retPromAlert.GroupLabels)
	jsonEncodeAlertmanagerKV(retPromAlert.CommonLabels)
	jsonEncodeAlertmanagerKV(retPromAlert.CommonAnnotations)
	for _, alert := range retPromAlert.Alerts {
		jsonEncodeAlertmanagerKV(alert.Labels)
		jsonEncodeAlertmanagerKV(alert.Annotations)
	}
	return retPromAlert
}

// ParseTemplateFile creates an Alertmanager template from the given file. The
// template can use Alertmanager's own functions and those of extraFuncs.
func ParseTemplateFile(f string) (*template.Template, error) {
	// Alertmanager builds every template with template.DefaultFuncs, a
	// package-level map; the extra functions are added to it.
	funcs := template.DefaultFuncs
	for k, v := range extraFuncs() {
		funcs[k] = v
	}
	template.DefaultFuncs = funcs

	if _, err := os.Stat(f); os.IsNotExist(err) {
		return nil, fmt.Errorf("template file %s does not exist", f)
	}

	tmpl, err := template.FromGlobs([]string{f})
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %v: %v", err, err)
	}

	return tmpl, nil
}

// extraFuncs are the template functions added to Alertmanager's own; where a
// name exists in both, this one is used:
//   - the sprig library (https://masterminds.github.io/sprig/), the same
//     functions Helm charts use, except env and expandenv
//   - toYaml and fromYaml, as in Helm
//   - counter: {{ $c := counter }}{{ call $c }} gives 0, 1, 2, ...
//   - mdEscape and labelMatchers, see below
//
// (Before, the functions came from Helm 2's template engine, which also
// had toToml; Helm 2 is no longer maintained.)
//
// Escaping belongs where a value is used, because each place needs its
// own: a card text needs mdEscape and then toJson, a link only toJson (or
// urlquery for a part of it). For example
//
//	"text": {{ .CommonAnnotations.summary | mdEscape | toJson }},
//	"url": {{ .CommonAnnotations.runbook | toJson }}
//
// (toJson writes the value as a JSON string, quotes included, so that
// quotes and line breaks in it cannot break the card.)
func extraFuncs() map[string]any {
	funcs := sprig.TxtFuncMap()
	// A template must not read the environment of this process: it may
	// hold secrets, and the card goes to a chat channel.
	delete(funcs, "env")
	delete(funcs, "expandenv")

	funcs["toYaml"] = toYaml
	funcs["fromYaml"] = fromYaml
	// counter returns a new function (a closure: it keeps its own i between
	// calls) that counts up from 0 each time it is called.
	funcs["counter"] = func() func() int {
		i := -1
		return func() int {
			i++
			return i
		}
	}
	funcs["mdEscape"] = mdEscape
	funcs["labelMatchers"] = labelMatchers
	return funcs
}

// markdownEscaper puts a backslash before each character that Teams'
// markdown in card texts would read as formatting: "_" and "*" (italics,
// bold), "[" and "]" (links), and the backslash itself.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	`*`, `\*`,
	`_`, `\_`,
	`[`, `\[`,
	`]`, `\]`,
)

// mdEscape makes s show as it is in a card text: "high_memory_load" stays
// that, instead of "high" + italic "memory" + "load".
func mdEscape(s string) string {
	return markdownEscaper.Replace(s)
}

// labelMatchers writes labels as an Alertmanager filter, the form the
// Alertmanager web interface takes in its "filter" parameter:
//
//	{alertname="HighLoad", severity="warning"}
//
// Names are sorted, so the result is the same each time. Values are quoted
// with Go's string quoting (strconv.Quote), which escapes quotes,
// backslashes and line breaks as Alertmanager expects. A name that is not a
// classic Prometheus label name is quoted too. In a URL, pass the result
// through urlquery.
func labelMatchers(labels map[string]string) string {
	// maps.Keys gives the names in random order (Go maps have none);
	// slices.Sorted collects and sorts them.
	names := slices.Sorted(maps.Keys(labels))
	matchers := make([]string, 0, len(names))
	for _, name := range names {
		n := name
		if !classicLabelName.MatchString(name) {
			n = strconv.Quote(name)
		}
		matchers = append(matchers, n+"="+strconv.Quote(labels[name]))
	}
	return "{" + strings.Join(matchers, ", ") + "}"
}

// classicLabelName is the form of a Prometheus label name that needs no
// quotes.
var classicLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// toYaml renders v as YAML; on error it gives "", as in Helm.
func toYaml(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// fromYaml parses a YAML mapping; on error the result holds only the key
// "Error" with the message, as in Helm.
func fromYaml(s string) map[string]any {
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}
