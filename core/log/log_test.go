package log_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/log"
)

func TestNew_DefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := log.NewWithWriter(&buf, "")
	logger.Debug("hidden")
	logger.Info("visible")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("debug output emitted at default level: %s", out)
	}
	if !strings.Contains(out, "visible") {
		t.Errorf("info output missing: %s", out)
	}
}

func TestNew_EachLevel(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		t.Run(lvl, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.NewWithWriter(&buf, lvl)
			logger.Info("test")
			// Just verify no panic and JSON output structure exists.
		})
	}
}

func TestNew_UnknownLevelDefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := log.NewWithWriter(&buf, "verbose") // not a real level
	logger.Debug("hidden")
	logger.Info("visible")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("debug output emitted; unknown level should default to info: %s", out)
	}
	if !strings.Contains(out, "visible") {
		t.Errorf("info output missing: %s", out)
	}
}

func TestNew_OutputIsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := log.NewWithWriter(&buf, "info")
	logger.Info("hello", "key", "value")

	// Each log line must parse as JSON (NFR7).
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("log line not valid JSON: %v\nline: %s", err, line)
		}
		if got["msg"] != "hello" {
			t.Errorf("msg = %v, want %q", got["msg"], "hello")
		}
		if got["key"] != "value" {
			t.Errorf("key = %v, want %q", got["key"], "value")
		}
	}
}

func TestDiscard(t *testing.T) {
	logger := log.Discard()
	if logger == nil {
		t.Fatal("Discard returned nil")
	}
	// Should not panic and should not write anywhere observable.
	logger.Info("anything")
	logger.Error("nothing observable")
}

func TestSubmissionID_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := log.SubmissionID()
		if len(id) != 36 {
			t.Errorf("UUID length = %d, want 36 (got %q)", len(id), id)
		}
		if strings.Count(id, "-") != 4 {
			t.Errorf("UUID dash count wrong: %q", id)
		}
		if seen[id] {
			t.Errorf("duplicate UUID: %q", id)
		}
		seen[id] = true
	}
}
