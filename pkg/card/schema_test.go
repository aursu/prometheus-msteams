package card

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aursu/prometheus-msteams/pkg/testutils"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The official Adaptive Card schema; see testdata/schema/README.md.
const (
	cardSchemaFile = "./testdata/schema/adaptive-card-1.5.json"
	// Teams renders cards up to this version; a higher "version" would make
	// Teams show only the card's fallbackText.
	maxCardMajor, maxCardMinor = 1, 5
)

// cardTemplates lists every card template in the repository that must
// produce schema-valid cards. Glob patterns are expanded.
var cardTemplates = []string{
	"../../default-message-workflow-card.tmpl",
	"./testdata/workflow_*.tmpl",
	"../../examples/templates/*workflow*.tmpl",
}

// compileCardSchema loads and compiles the schema file once per test.
func compileCardSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	f, err := os.Open(cardSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("adaptive-card.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("adaptive-card.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// alertStates are the alert groups every template is rendered with, one per
// branch a template usually has (colour by status and severity).
func alertStates(t *testing.T) map[string]func() webhook.Message {
	t.Helper()
	// Each entry builds a fresh copy of the sample message, because
	// rendering with escaped underscores modifies the message in place.
	load := func(status, severity string) func() webhook.Message {
		return func() webhook.Message {
			a, err := testutils.ParseWebhookJSONFromFile(testPromAlertFile)
			if err != nil {
				t.Fatal(err)
			}
			a.Status = status
			a.CommonLabels["severity"] = severity
			for i := range a.Alerts {
				a.Alerts[i].Status = status
				a.Alerts[i].Labels["severity"] = severity
			}
			return a
		}
	}
	return map[string]func() webhook.Message{
		"firing critical": load("firing", "critical"),
		"firing warning":  load("firing", "warning"),
		"firing info":     load("firing", "info"),
		"resolved":        load("resolved", "critical"),
	}
}

// Every card template in the repository, rendered for every alert state and
// with -auto-escape-underscores both on and off (both are valid settings),
// must produce Adaptive Cards that the official schema accepts.
func Test_cardTemplates_matchAdaptiveCardSchema(t *testing.T) {
	schema := compileCardSchema(t)

	var files []string
	for _, pattern := range cardTemplates {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Fatalf("no template matches %s", pattern)
		}
		files = append(files, matches...)
	}

	for _, file := range files {
		tmpl, err := ParseTemplateFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, escape := range []bool{true, false} {
			for state, newAlert := range alertStates(t) {
				name := fmt.Sprintf("%s/%s/escape=%t", filepath.Base(file), state, escape)
				t.Run(name, func(t *testing.T) {
					got, err := NewTemplatedCardCreator(tmpl, escape).Convert(t.Context(), newAlert())
					if err != nil {
						t.Fatalf("Convert: %v", err)
					}
					for i, card := range attachmentCards(t, got) {
						if err := validateCard(schema, card); err != nil {
							t.Errorf("attachment %d does not match the Adaptive Card schema:\n%v", i, err)
						}
					}
				})
			}
		}
	}
}

// attachmentCards returns the content of every attachment as raw JSON.
func attachmentCards(t *testing.T, msg json.RawMessage) []json.RawMessage {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(msg, &e); err != nil {
		t.Fatal(err)
	}
	cards := make([]json.RawMessage, 0, len(e.Attachments))
	for _, a := range e.Attachments {
		cards = append(cards, a.Content)
	}
	return cards
}

// validateCard checks one card against the schema, plus the two things the
// schema does not cover: the Teams extension "msteams" and the card version.
func validateCard(schema *jsonschema.Schema, card json.RawMessage) error {
	var obj map[string]interface{}
	if err := json.Unmarshal(card, &obj); err != nil {
		return err
	}

	// "msteams" is a Teams extension the schema does not know, and the
	// schema forbids unknown card properties. Check it is an object, then
	// validate the card without it. This is the only exception.
	if ext, ok := obj["msteams"]; ok {
		if _, isObject := ext.(map[string]interface{}); !isObject {
			return fmt.Errorf("msteams must be an object, got %T", ext)
		}
		delete(obj, "msteams")
	}

	if err := checkCardVersion(obj["version"]); err != nil {
		return err
	}

	// The validator needs the card in its own generic form.
	stripped, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(stripped))
	if err != nil {
		return err
	}
	return readableSchemaError(schema.Validate(inst))
}

// readableSchemaError shortens the validator's error to the real problems.
//
// The schema says a body element is "any of" some 20 element types, so for
// one bad TextBlock the validator explains why the element is not an
// ActionSet, not a ColumnSet, not an Image... and only once why it is not a
// valid TextBlock. Every branch whose complaint includes "the element's own
// type is not mine" is dropped, leaving the branch of the type the element
// declares. Each remaining problem is one line: "at '<path>': <message>".
func readableSchemaError(err error) error {
	// errors.As is Go's "is this error (or one it wraps) of type X"; on
	// success it also fills ve.
	var ve *jsonschema.ValidationError
	if err == nil || !errors.As(err, &ve) {
		return err
	}
	var lines []string
	collectProblems(ve, &lines)
	return errors.New(strings.Join(lines, "\n"))
}

// schemaErrorPrinter renders the validator's messages in English.
var schemaErrorPrinter = message.NewPrinter(language.English)

// collectProblems walks the error tree and appends one line per leaf error,
// skipping the branches that only describe a different element type.
func collectProblems(e *jsonschema.ValidationError, lines *[]string) {
	if len(e.Causes) == 0 {
		*lines = append(*lines, fmt.Sprintf("at '/%s': %s",
			strings.Join(e.InstanceLocation, "/"), e.ErrorKind.LocalizedString(schemaErrorPrinter)))
		return
	}
	var relevant []*jsonschema.ValidationError
	for _, c := range e.Causes {
		if !rejectsOwnType(c, c.InstanceLocation) {
			relevant = append(relevant, c)
		}
	}
	// An element of an unknown type is rejected by every branch; then
	// keep them all rather than report nothing.
	if len(relevant) == 0 {
		relevant = e.Causes
	}
	for _, c := range relevant {
		collectProblems(c, lines)
	}
}

// rejectsOwnType reports whether the error tree e contains "wrong value" for
// the "type" property of the element at location elem - i.e. e describes a
// different element type. Deeper elements (children) do not count.
func rejectsOwnType(e *jsonschema.ValidationError, elem []string) bool {
	loc := e.InstanceLocation
	if len(loc) == len(elem)+1 && loc[len(loc)-1] == "type" && slices.Equal(loc[:len(elem)], elem) {
		// A type switch: the branch taken depends on the concrete type
		// stored in the ErrorKind interface.
		switch e.ErrorKind.(type) {
		case *kind.Const, *kind.Enum:
			return true
		}
	}
	for _, c := range e.Causes {
		if rejectsOwnType(c, elem) {
			return true
		}
	}
	return false
}

// checkCardVersion requires a "major.minor" version not above the highest
// version Teams renders.
func checkCardVersion(v interface{}) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("card version must be a string like \"1.5\", got %v", v)
	}
	major, minor, found := strings.Cut(s, ".")
	maj, err1 := strconv.Atoi(major)
	minr, err2 := strconv.Atoi(minor)
	if !found || err1 != nil || err2 != nil {
		return fmt.Errorf("card version %q is not \"major.minor\"", s)
	}
	if maj > maxCardMajor || (maj == maxCardMajor && minr > maxCardMinor) {
		return fmt.Errorf("card version %s is above %d.%d, the highest Teams renders", s, maxCardMajor, maxCardMinor)
	}
	return nil
}
