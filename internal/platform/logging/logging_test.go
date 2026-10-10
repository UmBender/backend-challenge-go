package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// Three replicas write to the same log stream in Compose; these fields are
// what lets an operator tell them apart and filter by service.
func TestNewEmitsJSONTaggedWithServiceAndInstance(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "wallet-2").Info("hello", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %q (%v)", buf.String(), err)
	}
	for key, want := range map[string]string{"msg": "hello", "service": "wallet", "instanceId": "wallet-2", "k": "v"} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %q", key, rec[key], want)
		}
	}
}

func TestNewRespectsTheConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "wallet-1").Debug("noise")
	if buf.Len() != 0 {
		t.Fatalf("debug record written at info level: %q", buf.String())
	}
}
