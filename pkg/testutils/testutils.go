// Package testutils provides helper utilities for testing.
package testutils

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/alertmanager/notify/webhook"
)

// ParseWebhookJSONFromFile is a helper for parsing webhook data from JSON files.
func ParseWebhookJSONFromFile(f string) (webhook.Message, error) {
	b, err := os.ReadFile(f) //nolint:gosec
	if err != nil {
		return webhook.Message{}, err
	}
	var w webhook.Message
	if err := json.Unmarshal(b, &w); err != nil {
		return webhook.Message{}, err
	}
	return w, err
}

// CompareToGoldenFile compares the value of v to file in bytes.
// If update is true, it will update the the golden file using the value of v.
func CompareToGoldenFile(t *testing.T, v interface{}, file string, update bool) {
	gotBytes, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gp := filepath.Join("testdata", file)
	dir := filepath.Dir(gp)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		_ = os.MkdirAll(dir, 0750)
	}
	if _, err := os.Stat(gp); os.IsNotExist(err) {
		_ = os.WriteFile(gp, []byte{}, 0600)
	}
	if update {
		t.Log("updating golden file")
		if err := os.WriteFile(gp, gotBytes, 0600); err != nil {
			t.Fatalf("failed to update golden file: %s", err)
		}
	}
	want, err := os.ReadFile(gp) //nolint:gosec
	if err != nil {
		t.Fatalf("failed reading the golden file: %s", err)
	}
	if string(want) != string(gotBytes) {
		// Decode both sides into plain values (maps, slices, strings,
		// numbers), so that the diff names the fields that differ rather
		// than the bytes.
		var g, w any
		if err := json.Unmarshal(gotBytes, &g); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &w); err != nil {
			t.Fatalf("the golden file %s is not valid JSON: %s", gp, err)
		}

		result := fmt.Sprintf(
			"\ngot:\n%s\nwant:\n%s\ndiff (-want +got):\n%s",
			string(gotBytes),
			string(want),
			cmp.Diff(w, g),
		)

		t.Fatal(result)
	}
}
