// Package diag writes local, privacy-preserving diagnostics as newline
// delimited JSON. The desktop app, the MCP server and the API server share one
// log beside the index, coordinated with a Windows byte-range lock.
//
// Records carry durations, counts and error codes. They never contain file
// paths, search text, tool arguments, Docker names or raw error messages.
package diag

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const (
	fileLimit   = 2 * units.MiB
	recordLimit = 16 * units.KiB
	logName     = "space-review.jsonl"
	previousLog = "space-review.previous.jsonl"
)

// Code is an error whose text is a fixed, private-data-free code.
type Code string

func (code Code) Error() string { return string(code) }

type writer struct {
	dir   string
	limit int64
}

func (w writer) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit {
		return 0, errors.New("diagnostic record exceeds the log size limit")
	}
	// Both the desktop and MCP can append and rotate the same log.
	lock, err := os.OpenFile(filepath.Join(w.dir, "write.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	handle := windows.Handle(lock.Fd())
	var offset windows.Overlapped
	for attempt := 0; ; attempt++ {
		err = windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &offset)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || attempt == 4 {
			return 0, err
		}
		time.Sleep(time.Millisecond)
	}
	defer func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, &offset) }()

	path := filepath.Join(w.dir, logName)
	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if err == nil && info.Size()+int64(len(data)) > w.limit {
		if err := os.Rename(path, filepath.Join(w.dir, previousLog)); err != nil {
			return 0, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.Write(data)
}

type contextKey struct{}

type trace struct {
	logger    *slog.Logger
	emergency *slog.Logger
	operation uint64
}

var sequence atomic.Uint64

// queue keeps file writes off the caller's goroutine and drops records when full.
type queue struct {
	mu      sync.Mutex
	records chan []byte
	done    chan struct{}
	closed  bool
	dropped atomic.Uint64
}

func (q *queue) Write(data []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return len(data), nil
	}
	if int64(len(data)) <= recordLimit && len(q.records) < cap(q.records) {
		q.records <- bytes.Clone(data)
	} else {
		q.dropped.Add(1)
	}
	return len(data), nil
}

func (q *queue) close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.records)
	}
	q.mu.Unlock()
	select {
	case <-q.done:
	case <-time.After(500 * time.Millisecond):
	}
}

func (q *queue) drain(out writer, emergency *slog.Logger) {
	defer close(q.done)
	var failures int
	for entry := range q.records {
		if _, err := out.Write(entry); err != nil {
			failures++
			continue
		}
		if dropped := q.dropped.Swap(0); dropped > 0 || failures > 0 {
			emergency.Warn("diagnostics.records_dropped", "queue_dropped", dropped, "write_failures", failures)
			failures = 0
		}
	}
}

// Open starts logging beside the index. Level is info, debug or off. It
// returns a context that carries the logger, the diagnostics folder ("" when
// off) and a function that flushes and stops logging.
func Open(ctx context.Context, indexPath, mode, level string) (context.Context, string, func(), error) {
	if level == "off" {
		return ctx, "", func() {}, nil
	}
	dir := filepath.Join(filepath.Dir(indexPath), "diagnostics")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ctx, "", func() {}, err
	}
	threshold := slog.LevelInfo
	if level == "debug" {
		threshold = slog.LevelDebug
	}
	out := writer{dir: dir, limit: fileLimit}
	pending := &queue{records: make(chan []byte, 128), done: make(chan struct{})}
	fields := []any{"version", buildinfo.Version, "mode", mode, "pid", os.Getpid(), "session", time.Now().UnixNano()}
	current := trace{
		logger:    slog.New(slog.NewJSONHandler(pending, &slog.HandlerOptions{Level: threshold})).With(fields...),
		emergency: slog.New(slog.NewJSONHandler(out, nil)).With(fields...),
	}
	go pending.drain(out, current.emergency)
	return context.WithValue(ctx, contextKey{}, current), dir, pending.close, nil
}

// Inherit gives ctx the diagnostics carried by from. Servers use it for
// request contexts that do not derive from the process context.
func Inherit(ctx, from context.Context) context.Context {
	if current, ok := from.Value(contextKey{}).(trace); ok {
		return context.WithValue(ctx, contextKey{}, current)
	}
	return ctx
}

// Operation starts a new operation ID so related records can be grouped.
func Operation(ctx context.Context) context.Context {
	current, ok := ctx.Value(contextKey{}).(trace)
	if !ok {
		return ctx
	}
	current.operation = sequence.Add(1)
	return context.WithValue(ctx, contextKey{}, current)
}

// Log writes one record when logging is on and the level is enabled.
func Log(ctx context.Context, level slog.Level, event string, attrs ...slog.Attr) {
	current, ok := ctx.Value(contextKey{}).(trace)
	if !ok || !current.logger.Enabled(ctx, level) {
		return
	}
	if current.operation != 0 {
		attrs = append(attrs, slog.Uint64("op", current.operation))
	}
	current.logger.LogAttrs(ctx, level, event, attrs...)
}

// Milliseconds converts a duration for logging.
func Milliseconds(elapsed time.Duration) float64 { return float64(elapsed.Microseconds()) / 1000 }

// Duration logs how long an operation took and how it ended.
func Duration(ctx context.Context, event string, started time.Time, err error, attrs ...slog.Attr) {
	level, result := slog.LevelInfo, "ok"
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		result = "canceled"
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, model.ErrNotFound):
		result = "empty"
	default:
		level, result = slog.LevelWarn, "error"
		attrs = append(attrs, slog.String("error_kind", ErrorKind(err)))
	}
	Log(ctx, level, event, append(attrs,
		slog.Float64("duration_ms", Milliseconds(time.Since(started))), slog.String("result", result))...)
}

// ErrorKind names an error without its message. Error strings from the OS,
// the index and MCP can contain private input.
func ErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var code Code
	var classified *model.Error
	var sqliteCode interface{ Code() int }
	var errno syscall.Errno
	var syntax *json.SyntaxError
	var valueType *json.UnmarshalTypeError
	switch {
	case errors.As(err, &code):
		return string(code)
	case errors.As(err, &sqliteCode):
		return fmt.Sprintf("sqlite_%d", sqliteCode.Code())
	case errors.As(err, &errno):
		return fmt.Sprintf("win32_%d", errno)
	case errors.As(err, &syntax):
		return "invalid_json"
	case errors.As(err, &valueType):
		return "invalid_json_type"
	case errors.As(err, &classified):
		return fmt.Sprintf("kind_%d", classified.Kind)
	}
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(err) {
		err = inner
	}
	return fmt.Sprintf("%T", err)
}

// RecoverPanic logs a panic with a sanitized stack and panics again. Call it
// directly with defer: defer diag.RecoverPanic(ctx, "component").
func RecoverPanic(ctx context.Context, component string) {
	failure := recover()
	if failure == nil {
		return
	}
	if current, ok := ctx.Value(contextKey{}).(trace); ok {
		var locations [24]uintptr
		frames := runtime.CallersFrames(locations[:runtime.Callers(2, locations[:])])
		var stack []string
		for {
			frame, more := frames.Next()
			stack = append(stack, fmt.Sprintf("%s:%d %s", filepath.Base(frame.File), frame.Line, frame.Function))
			if !more {
				break
			}
		}
		current.emergency.ErrorContext(ctx, "app.panic", "op", current.operation, "component", component,
			"panic_type", fmt.Sprintf("%T", failure), "stack", stack)
	}
	panic(failure)
}
