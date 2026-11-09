package card

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Teams renders elements and properties that the open Adaptive Card schema
// does not know (https://learn.microsoft.com/microsoftteams/platform/
// task-modules-and-cards/cards/cards-format). The schema forbids unknown
// properties and element types, so validateCard handles them here first:
//
//   - a Teams-only element (Badge, Icon, CodeBlock) is checked against the
//     Teams documentation and must carry a "fallback": what Teams mobile and
//     any other client without it shows instead. The fallback takes the
//     element's place for the schema check, or the element is removed when
//     the fallback is "drop".
//   - a Teams-only property (showBorder, roundedCorners, maxHeight,
//     targetWidth) is checked - right element type, right kind of value - and removed.
//
// Microsoft publishes no JSON schema with these, so the rules below are
// written from the documentation; they catch typos and missing fallbacks,
// not every possible mistake.

// teamsElements checks each Teams-only element type.
var teamsElements = map[string]func(el map[string]any) error{
	"Badge":     checkBadge,
	"Icon":      checkIcon,
	"CodeBlock": checkCodeBlock,
}

// teamsProperty is a Teams-only property: the element types that may carry
// it (nil: any element) and a check of its value.
type teamsProperty struct {
	on    []string
	check func(v any) error
}

var teamsProperties = map[string]teamsProperty{
	"showBorder":     {on: []string{"Container", "ColumnSet", "Column"}, check: isBool},
	"roundedCorners": {on: []string{"Container", "ColumnSet", "Column", "Table"}, check: isBool},
	"maxHeight":      {on: []string{"Container", "Column", "TableCell"}, check: isPixels},
	"targetWidth":    {on: nil, check: isTargetWidth},
}

// replaceTeamsElements returns v with every Teams-only element replaced by
// its fallback and every Teams-only property removed. keep is false when v
// itself is an element whose fallback is "drop". at is the JSON path of v,
// for the error messages.
func replaceTeamsElements(v any, at string) (out any, keep bool, err error) {
	// A type switch: the branch depends on the dynamic type of v - a JSON
	// object decodes as map[string]any, an array as []any.
	switch node := v.(type) {
	case map[string]any:
		typ, _ := node["type"].(string)
		if check, ok := teamsElements[typ]; ok {
			if err := check(node); err != nil {
				return nil, false, fmt.Errorf("at '%s': %s: %w", at, typ, err)
			}
			switch fb := node["fallback"].(type) {
			case string:
				if fb != "drop" {
					return nil, false, fmt.Errorf("at '%s': %s: fallback must be \"drop\" or an element, got %q", at, typ, fb)
				}
				return nil, false, nil
			case map[string]any:
				return replaceTeamsElements(fb, at+"/fallback")
			default:
				return nil, false, fmt.Errorf("at '%s': %s needs a fallback for clients without it (Teams mobile)", at, typ)
			}
		}
		res := make(map[string]any, len(node))
		for k, val := range node {
			if p, ok := teamsProperties[k]; ok {
				if p.on != nil && !slices.Contains(p.on, typ) {
					return nil, false, fmt.Errorf("at '%s': %s is not a property of %s", at, k, typ)
				}
				if err := p.check(val); err != nil {
					return nil, false, fmt.Errorf("at '%s/%s': %w", at, k, err)
				}
				continue
			}
			child, keep, err := replaceTeamsElements(val, at+"/"+k)
			if err != nil {
				return nil, false, err
			}
			if keep {
				res[k] = child
			}
		}
		return res, true, nil
	case []any:
		res := make([]any, 0, len(node))
		for i, val := range node {
			child, keep, err := replaceTeamsElements(val, fmt.Sprintf("%s/%d", at, i))
			if err != nil {
				return nil, false, err
			}
			if keep {
				res = append(res, child)
			}
		}
		return res, true, nil
	default:
		return v, true, nil
	}
}

func checkBadge(el map[string]any) error {
	text, _ := el["text"].(string)
	icon, _ := el["icon"].(string)
	if text == "" && icon == "" {
		return fmt.Errorf("needs a text or an icon")
	}
	return checkEnums(el, map[string][]string{
		"style":      {"Default", "Subtle", "Informative", "Accent", "Good", "Attention", "Warning"},
		"appearance": {"Filled", "Tint"},
		"shape":      {"Square", "Rounded", "Circular"},
		"size":       {"Medium", "Large", "ExtraLarge"},
	})
}

func checkIcon(el map[string]any) error {
	if name, _ := el["name"].(string); name == "" {
		return fmt.Errorf("needs a name")
	}
	return checkEnums(el, map[string][]string{
		"size":  {"xxSmall", "xSmall", "Small", "Standard", "Medium", "Large", "xLarge", "xxLarge"},
		"color": {"Default", "Dark", "Light", "Accent", "Good", "Warning", "Attention"},
		"style": {"Filled", "Regular"},
	})
}

func checkCodeBlock(el map[string]any) error {
	if _, ok := el["codeSnippet"].(string); !ok {
		return fmt.Errorf("needs a codeSnippet")
	}
	if _, ok := el["language"]; !ok {
		return fmt.Errorf("needs a language")
	}
	if n, ok := el["startLineNumber"]; ok {
		if _, isNumber := n.(float64); !isNumber {
			return fmt.Errorf("startLineNumber must be a number, got %v", n)
		}
	}
	return checkEnums(el, map[string][]string{
		"language": {"Bash", "C", "C++", "C#", "CSS", "DOS", "Go", "GraphQL", "HTML", "Java", "JavaScript",
			"JSON", "Perl", "PHP", "PlainText", "PowerShell", "Python", "SQL", "TypeScript", "Verilog", "VHDL", "XML"},
	})
}

// checkEnums checks that each listed property, where present, has one of
// its allowed values.
func checkEnums(el map[string]any, allowed map[string][]string) error {
	for prop, values := range allowed {
		v, ok := el[prop]
		if !ok {
			continue
		}
		s, _ := v.(string)
		if !slices.Contains(values, s) {
			return fmt.Errorf("%s %v is not one of %s", prop, v, strings.Join(values, ", "))
		}
	}
	return nil
}

func isBool(v any) error {
	if _, ok := v.(bool); !ok {
		return fmt.Errorf("must be true or false, got %v", v)
	}
	return nil
}

var pixels = regexp.MustCompile(`^[0-9]+px$`)

func isPixels(v any) error {
	if s, _ := v.(string); !pixels.MatchString(s) {
		return fmt.Errorf("must be \"<number>px\", got %v", v)
	}
	return nil
}

// targetWidths are the card widths an element can be limited to: a width,
// or "atLeast:"/"atMost:" and a width. Teams documents them capitalised and
// writes them in lower case in its examples, so the case is not checked.
var targetWidths = []string{"VeryNarrow", "Narrow", "Standard", "Wide"}

func isTargetWidth(v any) error {
	s, _ := v.(string)
	// CutPrefix returns the rest of s and whether the prefix was there.
	if rest, ok := strings.CutPrefix(s, "atLeast:"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "atMost:"); ok {
		s = rest
	}
	for _, w := range targetWidths {
		if strings.EqualFold(s, w) {
			return nil
		}
	}
	return fmt.Errorf("must be one of %s, optionally after atLeast: or atMost:, got %v", strings.Join(targetWidths, ", "), v)
}

// checkToggleTargets checks that every element an Action.ToggleVisibility
// shows or hides exists: a wrong id is not a schema error, the button just
// does nothing.
func checkToggleTargets(card map[string]any) error {
	ids := map[string]bool{}
	var targets []string
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if id, ok := node["id"].(string); ok {
				ids[id] = true
			}
			if node["type"] == "Action.ToggleVisibility" {
				list, _ := node["targetElements"].([]any)
				for _, t := range list {
					// A target is an id, or an object with elementId.
					switch tt := t.(type) {
					case string:
						targets = append(targets, tt)
					case map[string]any:
						id, _ := tt["elementId"].(string)
						targets = append(targets, id)
					}
				}
			}
			for _, val := range node {
				walk(val)
			}
		case []any:
			for _, val := range node {
				walk(val)
			}
		}
	}
	walk(card)
	for _, t := range targets {
		if !ids[t] {
			return fmt.Errorf("Action.ToggleVisibility targets %q, but no element has that id", t)
		}
	}
	return nil
}

// The checks themselves: each case is a card body; the card around it is
// the same for all.
func Test_validateCard_teamsElements(t *testing.T) {
	schema := compileCardSchema(t)
	tests := []struct {
		name    string
		body    string
		wantErr string // "" = valid
	}{
		{
			name: "badge with fallback",
			body: `{"type":"Badge","text":"FIRING","style":"Attention","appearance":"Filled",
			        "fallback":{"type":"TextBlock","text":"FIRING"}}`,
		},
		{
			name:    "badge without fallback",
			body:    `{"type":"Badge","text":"FIRING"}`,
			wantErr: "needs a fallback",
		},
		{
			name:    "badge style typo",
			body:    `{"type":"Badge","text":"FIRING","style":"attention","fallback":"drop"}`,
			wantErr: "style attention is not one of",
		},
		{
			name:    "fallback is checked by the schema",
			body:    `{"type":"Badge","text":"FIRING","fallback":{"type":"TextBlock"}}`,
			wantErr: "text",
		},
		{
			name: "icon dropped",
			body: `{"type":"Icon","name":"Warning","color":"Warning","style":"Filled","fallback":"drop"}`,
		},
		{
			name: "code block",
			body: `{"type":"CodeBlock","codeSnippet":"kubectl get pods","language":"Bash",
			        "fallback":{"type":"TextBlock","text":"kubectl get pods","fontType":"Monospace"}}`,
		},
		{
			name:    "code block language typo",
			body:    `{"type":"CodeBlock","codeSnippet":"ls","language":"bash","fallback":"drop"}`,
			wantErr: "language bash is not one of",
		},
		{
			name: "container with border, corners and max height",
			body: `{"type":"Container","showBorder":true,"roundedCorners":true,"maxHeight":"200px",
			        "items":[{"type":"TextBlock","text":"x"}]}`,
		},
		{
			name:    "max height without px",
			body:    `{"type":"Container","maxHeight":"200","items":[{"type":"TextBlock","text":"x"}]}`,
			wantErr: "must be \"<number>px\"",
		},
		{
			name: "column limited to wider cards",
			body: `{"type":"ColumnSet","columns":[{"type":"Column","targetWidth":"atLeast:Standard","items":[{"type":"TextBlock","text":"x"}]}]}`,
		},
		{
			name:    "target width typo",
			body:    `{"type":"TextBlock","text":"x","targetWidth":"atleast:Phone"}`,
			wantErr: "must be one of VeryNarrow",
		},
		{
			name:    "border on a text block",
			body:    `{"type":"TextBlock","text":"x","showBorder":true}`,
			wantErr: "showBorder is not a property of TextBlock",
		},
		{
			name: "toggle with its target",
			body: `{"type":"ActionSet","actions":[{"type":"Action.ToggleVisibility","title":"Details","targetElements":["d"]}]},
			       {"type":"TextBlock","id":"d","text":"x","isVisible":false}`,
		},
		{
			name:    "toggle with a missing target",
			body:    `{"type":"ActionSet","actions":[{"type":"Action.ToggleVisibility","title":"Details","targetElements":["nope"]}]}`,
			wantErr: `targets "nope"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := `{"type":"AdaptiveCard","version":"1.5","body":[` + tt.body + `]}`
			err := validateCard(schema, []byte(card))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("want an error with %q, got %v", tt.wantErr, err)
			}
		})
	}
}
