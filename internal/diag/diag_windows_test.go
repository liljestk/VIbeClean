package diag

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/diag/diagtest"
	"github.com/liljestk/VIbeClean/internal/model"
)

func TestRotationAndConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	out := writer{dir: dir, limit: 120}
	write := func(id int) error {
		_, err := fmt.Fprintf(out, "{\"id\":%d,\"padding\":%q}\n", id, strings.Repeat("x", 72))
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
	if _, err := out.Write(make([]byte, out.limit+1)); err == nil {
		t.Fatal("an oversized record should be rejected")
	}
	seen := map[int]bool{}
	for _, name := range []string{logName, previousLog} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var entry struct{ ID int }
		if int64(len(data)) > out.limit || json.Unmarshal(data, &entry) != nil {
			t.Fatalf("%s is oversized or contains an incomplete JSON record: %q", name, data)
		}
		seen[entry.ID] = true
	}
	if !seen[3] || !seen[4] {
		t.Fatalf("rotation should keep the two latest complete records, got %v", seen)
	}
}

func TestWriterReturnsWhenAnotherWriterHoldsLock(t *testing.T) {
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
	defer func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, &offset) }()
	out := writer{dir: dir, limit: fileLimit}
	done := make(chan error, 1)
	go func() {
		_, err := out.Write([]byte("{}\n"))
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

func TestQueueBoundsAndFlushes(t *testing.T) {
	dir := t.TempDir()
	pending := &queue{records: make(chan []byte, 1), done: make(chan struct{})}
	entry := []byte("{\"msg\":\"retained\"}\n")
	_, _ = pending.Write(make([]byte, recordLimit+1))
	_, _ = pending.Write(entry)
	entry[0] = 0
	_, _ = pending.Write([]byte("{\"msg\":\"overflow\"}\n"))
	out := writer{dir: dir, limit: fileLimit}
	go pending.drain(out, slog.New(slog.NewJSONHandler(out, nil)))
	pending.close()
	pending.close()
	_, _ = pending.Write([]byte("{\"msg\":\"after-close\"}\n"))
	events, _ := diagtest.ReadEvents(t, dir)
	if len(events) != 2 || events[0].Message != "retained" || events[1].Message != "diagnostics.records_dropped" || events[1].QueueDropped != 2 {
		t.Fatalf("unexpected queue output: %+v", events)
	}
}

func TestLevelsPrivacyAndCrash(t *testing.T) {
	for _, level := range []string{"info", "debug", "off"} {
		t.Run(level, func(t *testing.T) {
			root := t.TempDir()
			ctx, dir, closeLog, err := Open(context.Background(), filepath.Join(root, "index.sqlite3"), "test", level)
			if err != nil {
				t.Fatal(err)
			}
			defer closeLog()
			ctx = Operation(ctx)
			Log(ctx, slog.LevelDebug, "test.debug")
			Duration(ctx, "test.failure", time.Now(),
				fmt.Errorf("%s: %w", diagtest.PrivateMarker, &os.PathError{Path: diagtest.PrivateMarker, Err: windows.ERROR_ACCESS_DENIED}))
			Duration(ctx, "test.cancel", time.Now(), context.Canceled)
			Duration(ctx, "test.empty", time.Now(), sql.ErrNoRows)
			Duration(ctx, "test.missing", time.Now(), model.NotFound("%s", diagtest.PrivateMarker))
			Duration(ctx, "test.conflict", time.Now(), model.Conflict("%s", diagtest.PrivateMarker))
			func() {
				defer func() {
					if got := recover(); got != diagtest.PrivateMarker {
						t.Fatalf("panic did not propagate: %v", got)
					}
				}()
				defer RecoverPanic(ctx, "test")
				panic(diagtest.PrivateMarker)
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
			events, raw := diagtest.ReadEvents(t, dir)
			if strings.Contains(raw, diagtest.PrivateMarker) {
				t.Fatal("an error or panic logged private input")
			}
			byName := map[string]diagtest.Event{}
			for _, event := range events {
				byName[event.Message] = event
				if event.Time == "" || event.Operation == 0 || event.Session == 0 || event.Mode != "test" || event.Version != buildinfo.Version {
					t.Fatalf("missing log context: %+v", event)
				}
			}
			if _, debug := byName["test.debug"]; debug != (level == "debug") {
				t.Fatalf("debug visibility for %s: %t", level, debug)
			}
			if row := byName["test.failure"]; row.Result != "error" || row.ErrorKind != "win32_5" || row.Duration == nil {
				t.Fatalf("missing error timing or code: %+v", row)
			}
			if byName["test.cancel"].Result != "canceled" || byName["test.empty"].Result != "empty" ||
				byName["test.missing"].Result != "empty" || byName["test.conflict"].ErrorKind != "kind_3" {
				t.Fatalf("unexpected normal outcomes: %+v", byName)
			}
			if row := byName["app.panic"]; row.Level != "ERROR" || len(row.Stack) == 0 {
				t.Fatalf("panic location missing: %+v", row)
			}
		})
	}
}
