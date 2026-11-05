package card

import (
	"strings"
	"testing"
)

// The functions a card template can use beyond Alertmanager's own.
func Test_extraFuncs(t *testing.T) {
	tmpl, err := ParseTemplateFile(writeTemplate(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		text    string
		want    string
		prefix  bool   // got only has to start with want
		wantErr string // part of the error; "" = no error expected
	}{
		{name: "sprig", text: `{{ "abc" | upper }} {{ default "none" "" }}`, want: "ABC none"},
		{name: "toJson", text: `{{ list 1 "a" | toJson }}`, want: `[1,"a"]`},
		{name: "fromJson", text: `{{ (fromJson "{\"a\": 1}").a }}`, want: "1"},
		{name: "toYaml", text: `{{ dict "a" 1 | toYaml }}`, want: "a: 1\n"},
		{name: "fromYaml", text: `{{ (fromYaml "a: b").a }}`, want: "b"},
		{name: "fromYaml error", text: `{{ (fromYaml "a: [").Error }}`, want: "yaml: ", prefix: true},
		{name: "counter", text: `{{ $c := counter }}{{ call $c }}{{ call $c }}{{ call $c }}`, want: "012"},
		{name: "Alertmanager's own", text: `{{ "a_b" | reReplaceAll "_" "-" }}`, want: "a-b"},
		// A template must not read the environment of the process.
		{name: "no env", text: `{{ env "HOME" }}`, wantErr: `"env" not defined`},
		{name: "no expandenv", text: `{{ expandenv "$HOME" }}`, wantErr: `"expandenv" not defined`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// ExecuteTextString parses text with the template's functions
			// and runs it.
			got, err := tmpl.ExecuteTextString(tt.text, nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want && !(tt.prefix && strings.HasPrefix(got, tt.want)) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
