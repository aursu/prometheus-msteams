package card

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/aursu/prometheus-msteams/pkg/testutils"
)

// The full alert and card are logged at debug level only.
func TestLoggingMiddleware(t *testing.T) {
	tmpl, err := ParseTemplateFile("../../default-message-workflow-card.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	a, err := testutils.ParseWebhookJSONFromFile(testPromAlertFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
			c := NewCreatorLoggingMiddleware(logger, NewTemplatedCardCreator(tmpl, false))

			if _, err := c.Convert(context.Background(), a); err != nil {
				t.Fatal(err)
			}

			logged := strings.Contains(buf.String(), "card built") && strings.Contains(buf.String(), "card=")
			if want := level == slog.LevelDebug; logged != want {
				t.Errorf("at level %s the card was logged: %t, want %t\n%s", level, logged, want, buf.String())
			}
		})
	}
}
