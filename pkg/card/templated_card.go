package card

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
	"go.opencensus.io/trace"
	"k8s.io/helm/pkg/engine"
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
func (m *templatedCard) Convert(ctx context.Context, promAlert webhook.Message) (json.RawMessage, error) {
	_, span := trace.StartSpan(ctx, "templatedCard.Convert")
	defer span.End()

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

/*
	ParseTemplateFile creates an alertmanager template from the given file.

The functions include all functions (except 'env' and 'expandenv' ) from sprig (http://masterminds.github.io/sprig/)
and the following functions from HELM templating:
  - toToml
  - toYaml
  - fromYaml
  - toJson
  - fromJson
*/
func ParseTemplateFile(f string) (*template.Template, error) {
	funcs := template.DefaultFuncs
	for k, v := range engine.FuncMap() {
		funcs[k] = v
	}
	funcs["counter"] = func() func() int {
		i := -1
		return func() int {
			i++
			return i
		}
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
