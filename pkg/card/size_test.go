package card

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/aursu/prometheus-msteams/pkg/testutils"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/template"
)

const defaultTemplatePath = "../../default-message-workflow-card.tmpl"

// defaultCard returns the plain converter with the default template.
func defaultCard(t *testing.T) Converter {
	t.Helper()
	tmpl, err := ParseTemplateFile(defaultTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	return NewTemplatedCardCreator(tmpl, false)
}

// bigGroup returns the sample message with n alerts: the first half
// resolved, the second half firing, each with its own instance label.
func bigGroup(t *testing.T, n int) webhook.Message {
	t.Helper()
	msg, err := testutils.ParseWebhookJSONFromFile(testPromAlertFile)
	if err != nil {
		t.Fatal(err)
	}
	sample := msg.Alerts[0]
	alerts := make(template.Alerts, 0, n)
	for i := range n {
		a := sample
		a.Labels = maps.Clone(sample.Labels)
		a.Annotations = maps.Clone(sample.Annotations)
		a.Status = "firing"
		if i < n/2 {
			a.Status = "resolved"
		}
		a.Labels["instance"] = fmt.Sprintf("%s-%03d", a.Status, i)
		alerts = append(alerts, a)
	}
	msg.Alerts = alerts
	return msg
}

// shownAlerts counts the alerts in a default-template card: a group has a
// table with a header row and one row per alert, a single alert no table.
func shownAlerts(t *testing.T, c json.RawMessage) int {
	t.Helper()
	tables := findElements(firstCard(t, c), "Table")
	if len(tables) == 0 {
		return 1
	}
	rows, _ := tables[0]["rows"].([]interface{})
	return len(rows) - 1
}

// A card within the limit is the same as without the limiter.
func Test_sizeLimiter_smallCard(t *testing.T) {
	msg, err := testutils.ParseWebhookJSONFromFile(testPromAlertFile)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	limited, err := NewSizeLimiter(slog.New(slog.NewTextHandler(&logs, nil)), defaultCard(t), DefaultMaxSize).Convert(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := defaultCard(t).Convert(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(limited, plain) {
		t.Errorf("card changed by the limiter:\n%s\nwant\n%s", limited, plain)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected log: %s", logs.String())
	}
}

// A large group gives a card within the limit with as many alerts as fit,
// firing ones first, and a line saying how many are left out (counting the
// ones Alertmanager already cut).
func Test_sizeLimiter_largeGroup(t *testing.T) {
	const total, cutByAlertmanager = 300, 5
	msg := bigGroup(t, total)
	msg.TruncatedAlerts = cutByAlertmanager

	var logs bytes.Buffer
	limiter := NewSizeLimiter(slog.New(slog.NewTextHandler(&logs, nil)), defaultCard(t), DefaultMaxSize)
	c, err := limiter.Convert(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}

	if len(c) > DefaultMaxSize {
		t.Errorf("card is %d bytes, limit %d", len(c), DefaultMaxSize)
	}
	shown := shownAlerts(t, c)
	if shown < 1 || shown >= total/2 {
		t.Fatalf("card shows %d alerts, want between 1 and %d", shown, total/2-1)
	}
	if strings.Contains(string(c), "resolved-") {
		t.Error("card shows a resolved alert while firing ones were left out")
	}
	wantLine := fmt.Sprintf("%d more alerts not shown", total-shown+cutByAlertmanager)
	if !strings.Contains(string(c), wantLine) {
		t.Errorf("card has no %q line:\n%s", wantLine, c)
	}
	if !strings.Contains(logs.String(), "card too large") {
		t.Errorf("no warning logged: %s", logs.String())
	}

	// As many as fit: one alert more would not.
	more, err := defaultCard(t).Convert(context.Background(), withAlerts(msg, firingFirst(msg.Alerts)[:shown+1], 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(more) <= DefaultMaxSize {
		t.Errorf("%d alerts would fit too (%d bytes)", shown+1, len(more))
	}

	// The caller's message is unchanged.
	if len(msg.Alerts) != total || msg.TruncatedAlerts != cutByAlertmanager || msg.Alerts[0].Status != "resolved" {
		t.Errorf("message changed: %d alerts, %d truncated, first %s", len(msg.Alerts), msg.TruncatedAlerts, msg.Alerts[0].Status)
	}
}

// When not even one alert fits, the result is an error, not a card Teams
// would refuse.
func Test_sizeLimiter_tooLarge(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d alerts", n), func(t *testing.T) {
			c, err := NewSizeLimiter(slog.New(slog.DiscardHandler), defaultCard(t), 100).Convert(context.Background(), bigGroup(t, n))
			if err == nil || !strings.Contains(err.Error(), "limit of 100") {
				t.Fatalf("Convert() = %d bytes, error %v; want an error naming the limit", len(c), err)
			}
		})
	}
}

// maxSize 0 turns the limit off.
func Test_sizeLimiter_off(t *testing.T) {
	c, err := NewSizeLimiter(slog.New(slog.DiscardHandler), defaultCard(t), 0).Convert(context.Background(), bigGroup(t, 300))
	if err != nil {
		t.Fatal(err)
	}
	if got := shownAlerts(t, c); got != 300 {
		t.Errorf("card shows %d alerts, want all 300", got)
	}
}

// With -auto-escape-underscores the message is escaped in a copy: rendering
// it twice gives the same card, and the message keeps its values.
func Test_escapeUnderscores_leavesMessage(t *testing.T) {
	tmpl, err := ParseTemplateFile(defaultTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	c := NewTemplatedCardCreator(tmpl, true)
	msg := bigGroup(t, 2)

	first, err := c.Convert(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Convert(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second rendering differs:\n%s\nfirst\n%s", second, first)
	}
	if got := msg.Alerts[0].Labels["job"]; got != "docker_nodes" {
		t.Errorf("label changed to %q", got)
	}
	if got := msg.CommonLabels["alertname"]; got != "high_memory_load" {
		t.Errorf("common label changed to %q", got)
	}
}
