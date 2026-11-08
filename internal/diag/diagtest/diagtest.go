// Package diagtest reads diagnostics logs in tests.
package diagtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PrivateMarker is test input that must never appear in a log.
const PrivateMarker = "private-diagnostic-test-input"

// Event is the subset of a log record that tests inspect.
type Event struct {
	Message       string   `json:"msg"`
	Time          string   `json:"time"`
	Level         string   `json:"level"`
	Version       string   `json:"version"`
	Mode          string   `json:"mode"`
	Session       int64    `json:"session"`
	Operation     uint64   `json:"op"`
	Duration      *float64 `json:"duration_ms"`
	Result        string   `json:"result"`
	ErrorKind     string   `json:"error_kind"`
	Tool          string   `json:"tool"`
	Method        string   `json:"method"`
	ScopeType     string   `json:"scope_type"`
	CaptureID     int64    `json:"capture_id"`
	FilesChecked  int64    `json:"files_checked"`
	Endpoint      string   `json:"endpoint"`
	Status        int      `json:"http_status"`
	ResponseBytes int64    `json:"response_bytes"`
	QueueDropped  uint64   `json:"queue_dropped"`
	Stack         []string `json:"stack"`
}

// ReadEvents parses the active log in dir and also returns its raw text.
func ReadEvents(t *testing.T, dir string) ([]Event, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "space-review.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	var events []Event
	for _, line := range strings.Split(text, "\n") {
		var entry Event
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid log record %q: %v", line, err)
		}
		events = append(events, entry)
	}
	return events, text
}

// CreateFile makes a file of the given logical size, creating its folders.
func CreateFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(size)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
