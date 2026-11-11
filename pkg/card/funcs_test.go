package card

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/alertmanager/template"
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

// fixClock makes now and since in templates see the time at, until the
// test ends.
func fixClock(t *testing.T, at time.Time) {
	t.Helper()
	old := clock
	clock = func() time.Time { return at }
	t.Cleanup(func() { clock = old })
}

// now and since in a template follow the package clock.
func Test_clockFuncs(t *testing.T) {
	tmpl, err := ParseTemplateFile(writeTemplate(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	fixClock(t, time.Date(2026, 11, 9, 20, 44, 0, 0, time.UTC))
	data := map[string]any{"Start": time.Date(2026, 11, 9, 20, 34, 0, 0, time.UTC)}
	got, err := tmpl.ExecuteTextString(`{{ now.Format "15:04" }} {{ since .Start | shortDuration }}`, data)
	if err != nil {
		t.Fatal(err)
	}
	if want := "20:44 10 min"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func Test_shortDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "less than a minute"},
		{in: 59 * time.Second, want: "less than a minute"},
		{in: time.Minute, want: "1 min"},
		{in: 10*time.Minute + 59*time.Second, want: "10 min"},
		{in: time.Hour, want: "1 h"},
		{in: 2*time.Hour + 5*time.Minute, want: "2 h 5 min"},
		{in: 24 * time.Hour, want: "1 d"},
		{in: 3*24*time.Hour + 4*time.Hour + 30*time.Minute, want: "3 d 4 h"},
		{in: -time.Minute, want: "less than a minute"},
	}
	for _, tt := range tests {
		if got := shortDuration(tt.in); got != tt.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func Test_mdEscape(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "plain text", want: "plain text"},
		{in: "high_memory_load", want: `high\_memory\_load`},
		{in: "**bold** and *it*", want: `\*\*bold\*\* and \*it\*`},
		{in: "[text](https://example.org/a_b)", want: `\[text\](https://example.org/a\_b)`},
		{in: `C:\path`, want: `C:\\path`},
		{in: `already \_ escaped`, want: `already \\\_ escaped`},
		{in: "", want: ""},
	}
	for _, tt := range tests {
		if got := mdEscape(tt.in); got != tt.want {
			t.Errorf("mdEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func Test_labelMatchers(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{name: "sorted by name", labels: map[string]string{"severity": "warning", "alertname": "HighLoad"}, want: `{alertname="HighLoad", severity="warning"}`},
		{name: "underscores stay", labels: map[string]string{"job": "node_exporter"}, want: `{job="node_exporter"}`},
		{name: "quote and backslash", labels: map[string]string{"msg": `say "hi" \o/`}, want: `{msg="say \"hi\" \\o/"}`},
		{name: "line break", labels: map[string]string{"msg": "a\nb"}, want: `{msg="a\nb"}`},
		{name: "name that needs quotes", labels: map[string]string{"service.name": "api"}, want: `{"service.name"="api"}`},
		{name: "no labels", labels: map[string]string{}, want: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelMatchers(tt.labels); got != tt.want {
				t.Errorf("labelMatchers() = %s, want %s", got, tt.want)
			}
		})
	}
}

// urlComponent: a space is %20, never "+" - the part after "#" of a link
// does not read "+" as a space - and a "+" of the value is %2B.
func Test_urlComponent(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "space", in: `{a="x", b="y"}`, want: `%7Ba%3D%22x%22%2C%20b%3D%22y%22%7D`},
		{name: "plus stays a plus", in: "1+1", want: "1%2B1"},
		{name: "unreserved unchanged", in: "a-b_c.d~e", want: "a-b_c.d~e"},
		{name: "reserved escaped", in: "a/b?c&d#e", want: "a%2Fb%3Fc%26d%23e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := urlComponent(tt.in)
			if got != tt.want {
				t.Errorf("urlComponent(%q) = %s, want %s", tt.in, got, tt.want)
			}
			// Read back as a browser reads the part after "#".
			if back, err := url.PathUnescape(got); err != nil || back != tt.in {
				t.Errorf("read back = %q (%v), want %q", back, err, tt.in)
			}
		})
	}
}

// The functions are available in templates, also with Alertmanager's own
// label type (template.KV) as the argument.
func Test_escapeFuncsInTemplates(t *testing.T) {
	tmpl, err := ParseTemplateFile(writeTemplate(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"Labels": template.KV{"alertname": "a_b", "job": `x"y`}}

	got, err := tmpl.ExecuteTextString(`{{ .Labels.alertname | mdEscape | toJson }} {{ labelMatchers .Labels | urlComponent }}`, data)
	if err != nil {
		t.Fatal(err)
	}
	// toJson doubles the backslash: in JSON, \\ stands for one \.
	want := `"a\\_b" %7Balertname%3D%22a_b%22%2C%20job%3D%22x%5C%22y%22%7D`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
