package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const privateLogMarker = "private-diagnostic-test-input"

type diagnosticEvent struct {
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
	ScopeType     string   `json:"scope_type"`
	CaptureID     int64    `json:"capture_id"`
	FilesChecked  int64    `json:"files_checked"`
	Endpoint      string   `json:"endpoint"`
	Status        int      `json:"http_status"`
	ResponseBytes int64    `json:"response_bytes"`
	QueueDropped  uint64   `json:"queue_dropped"`
	Stack         []string `json:"stack"`
}

func readDiagnosticEvents(t *testing.T, dir string) ([]diagnosticEvent, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "space-review.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	var events []diagnosticEvent
	for _, line := range strings.Split(text, "\n") {
		var entry diagnosticEvent
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid log record %q: %v", line, err)
		}
		events = append(events, entry)
	}
	return events, text
}

func TestDiagnosticRotationAndConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	writer := diagnosticWriter{dir: dir, limit: 120}
	write := func(id int) error {
		_, err := fmt.Fprintf(writer, "{\"id\":%d,\"padding\":%q}\n", id, strings.Repeat("x", 72))
		return err
	}
	for _, id := range []int{1, 2} {
		if err := write(id); err != nil {
			t.Fatal(err)
		}
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for _, id := range []int{3, 4} {
		go func() {
			<-start
			results <- write(id)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Write(make([]byte, writer.limit+1)); err == nil {
		t.Fatal("an oversized record should be rejected")
	}
	seen := map[int]bool{}
	for _, name := range []string{"space-review.jsonl", "space-review.previous.jsonl"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var entry struct{ ID int }
		if int64(len(data)) > writer.limit || json.Unmarshal(data, &entry) != nil {
			t.Fatalf("%s is oversized or contains an incomplete JSON record: %q", name, data)
		}
		seen[entry.ID] = true
	}
	if !seen[3] || !seen[4] {
		t.Fatalf("rotation should keep the two latest complete records, got %v", seen)
	}
}

func TestDiagnosticWriterReturnsWhenAnotherWriterHoldsLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "write.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	handle := windows.Handle(lock.Fd())
	var offset windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &offset); err != nil {
		t.Fatal(err)
	}
	defer windows.UnlockFileEx(handle, 0, 1, 0, &offset)
	writer := diagnosticWriter{dir: dir, limit: diagnosticFileLimit}
	done := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte("{}\n"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			t.Fatalf("expected a lock violation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostic write blocked on another process")
	}
}

func TestDiagnosticQueueBoundsAndFlushes(t *testing.T) {
	dir := t.TempDir()
	queue := &diagnosticQueue{records: make(chan []byte, 1), done: make(chan struct{})}
	entry := []byte("{\"msg\":\"retained\"}\n")
	_, _ = queue.Write(make([]byte, diagnosticRecordLimit+1))
	_, _ = queue.Write(entry)
	entry[0] = 0
	_, _ = queue.Write([]byte("{\"msg\":\"overflow\"}\n"))
	writer := diagnosticWriter{dir: dir, limit: diagnosticFileLimit}
	go queue.drain(writer, slog.New(slog.NewJSONHandler(writer, nil)))
	queue.close()
	queue.close()
	_, _ = queue.Write([]byte("{\"msg\":\"after-close\"}\n"))
	events, _ := readDiagnosticEvents(t, dir)
	if len(events) != 2 || events[0].Message != "retained" || events[1].Message != "diagnostics.records_dropped" || events[1].QueueDropped != 2 {
		t.Fatalf("unexpected queue output: %+v", events)
	}
}

func TestDiagnosticsLevelsPrivacyAndCrash(t *testing.T) {
	for _, level := range []string{"info", "debug", "off"} {
		t.Run(level, func(t *testing.T) {
			root := t.TempDir()
			ctx, dir, closeLog, err := openDiagnostics(context.Background(), filepath.Join(root, "index.sqlite3"), "test", level)
			if err != nil {
				t.Fatal(err)
			}
			defer closeLog()
			ctx = diagnosticOperation(ctx)
			logDiagnostic(ctx, slog.LevelDebug, "test.debug")
			logDuration(ctx, "test.failure", time.Now(),
				fmt.Errorf("%s: %w", privateLogMarker, &os.PathError{Path: privateLogMarker, Err: windows.ERROR_ACCESS_DENIED}))
			logDuration(ctx, "test.cancel", time.Now(), context.Canceled)
			logDuration(ctx, "test.empty", time.Now(), sql.ErrNoRows)
			func() {
				defer func() {
					if got := recover(); got != privateLogMarker {
						t.Fatalf("panic did not propagate: %v", got)
					}
				}()
				defer logPanic(ctx, "test")
				panic(privateLogMarker)
			}()
			closeLog()
			if level == "off" {
				if dir != "" {
					t.Fatalf("logging off returned a directory: %s", dir)
				}
				if _, err := os.Stat(filepath.Join(root, "diagnostics")); !os.IsNotExist(err) {
					t.Fatalf("logging off created a directory: %v", err)
				}
				return
			}
			events, raw := readDiagnosticEvents(t, dir)
			if strings.Contains(raw, privateLogMarker) {
				t.Fatal("an error or panic logged private input")
			}
			byName := map[string]diagnosticEvent{}
			for _, event := range events {
				byName[event.Message] = event
				if event.Time == "" || event.Operation == 0 || event.Session == 0 || event.Mode != "test" || event.Version != appVersion {
					t.Fatalf("missing log context: %+v", event)
				}
			}
			if _, debug := byName["test.debug"]; debug != (level == "debug") {
				t.Fatalf("debug visibility for %s: %t", level, debug)
			}
			if row := byName["test.failure"]; row.Result != "error" || row.ErrorKind != "win32_5" || row.Duration == nil {
				t.Fatalf("missing error timing or code: %+v", row)
			}
			if byName["test.cancel"].Result != "canceled" || byName["test.empty"].Result != "empty" {
				t.Fatalf("unexpected normal outcomes: %+v", byName)
			}
			if row := byName["app.panic"]; row.Level != "ERROR" || len(row.Stack) == 0 {
				t.Fatalf("panic location missing: %+v", row)
			}
		})
	}
}

func TestCanceledCaptureLogsWorkAndPreservesHistory(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index.sqlite3")
	ctx, dir, closeLog, err := openDiagnostics(context.Background(), indexPath, "test", "info")
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	idx, err := openIndex(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.db.Close()
	folder := filepath.Join(root, privateLogMarker)
	createFile(t, filepath.Join(folder, "project", "file.bin"), 4*mib)
	baseline, err := runCapture(diagnosticOperation(ctx), idx, folder, "", nil)
	if err != nil || baseline.ID == 0 {
		t.Fatalf("could not create the baseline: id=%d, err=%v", baseline.ID, err)
	}
	captureCtx, cancel := context.WithCancel(diagnosticOperation(ctx))
	defer cancel()
	canceled, err := runCapture(captureCtx, idx, folder, "", func(string, int64) { cancel() })
	if !errors.Is(err, context.Canceled) || canceled.ID != 0 {
		t.Fatalf("canceled capture returned id=%d, err=%v", canceled.ID, err)
	}
	diff, err := idx.compare(ctx, baseline.Scope, 0)
	if err != nil || diff.Latest.ID != baseline.ID || diff.Baseline.ID != 0 {
		t.Fatalf("cancellation changed saved history: %+v, %v", diff, err)
	}
	closeLog()
	events, raw := readDiagnosticEvents(t, dir)
	if strings.Contains(raw, privateLogMarker) {
		t.Fatal("cancellation logged a private path")
	}
	var cancelOp uint64
	for _, event := range events {
		if event.Message == "capture.finish" && event.Result == "canceled" {
			cancelOp = event.Operation
		}
	}
	seenScan := false
	for _, event := range events {
		if event.Operation != cancelOp {
			continue
		}
		if event.Message == "scan.files" && event.Result == "canceled" {
			seenScan = true
		}
		if event.Message == "index.save" {
			t.Fatal("a canceled capture reached the index write")
		}
	}
	if cancelOp == 0 || !seenScan {
		t.Fatal("the trace is missing the canceled operation or its scan")
	}
}

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDockerAPIDiagnostics(t *testing.T) {
	ctx, dir, closeLog, err := openDiagnostics(context.Background(), filepath.Join(t.TempDir(), "index.sqlite3"), "test", "info")
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	ctx = diagnosticOperation(ctx)
	for _, tc := range []struct {
		status int
		body   string
		failed bool
	}{
		{http.StatusOK, "{\"api\":\"local\"}", false},
		{http.StatusServiceUnavailable, privateLogMarker, true},
		{http.StatusOK, privateLogMarker, true},
	} {
		client := &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.Path != "/system/df" {
				t.Fatalf("unexpected request: %v %v", r.Method, r.URL)
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}
		var output map[string]string
		if err := dockerJSON(ctx, client, "/system/df", &output); (err != nil) != tc.failed {
			t.Fatalf("unexpected Docker request result: %v", err)
		}
	}
	closeLog()
	events, raw := readDiagnosticEvents(t, dir)
	if strings.Contains(raw, privateLogMarker) || len(events) != 3 {
		t.Fatalf("unexpected diagnostic records: %+v", events)
	}
	for _, event := range events {
		if event.Message != "docker.api" || event.Endpoint != "/system/df" || event.Duration == nil {
			t.Fatalf("missing API diagnostics: %+v", event)
		}
	}
	if events[0].Status != 200 || events[0].Result != "ok" || events[0].ResponseBytes == 0 ||
		events[1].Status != 503 || events[1].Result != "error" || events[2].ErrorKind != "invalid_json" {
		t.Fatalf("unexpected API results: %+v", events)
	}
}
