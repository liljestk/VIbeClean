package main

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
)

const (
	diagnosticFileLimit   = 2 * mib
	diagnosticRecordLimit = 16 * 1024
)

type diagnosticWriter struct {
	dir   string
	limit int64
}

func (w diagnosticWriter) Write(data []byte) (int, error) {
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
	defer windows.UnlockFileEx(handle, 0, 1, 0, &offset)

	path := filepath.Join(w.dir, "space-review.jsonl")
	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if err == nil && info.Size()+int64(len(data)) > w.limit {
		if err := os.Rename(path, filepath.Join(w.dir, "space-review.previous.jsonl")); err != nil {
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

type diagnosticsKey struct{}

type diagnosticTrace struct {
	logger    *slog.Logger
	emergency *slog.Logger
	operation uint64
}

var diagnosticSequence atomic.Uint64

type diagnosticQueue struct {
	mu      sync.Mutex
	records chan []byte
	done    chan struct{}
	closed  bool
	dropped atomic.Uint64
}

func (q *diagnosticQueue) Write(data []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return len(data), nil
	}
	if len(data) <= diagnosticRecordLimit && len(q.records) < cap(q.records) {
		q.records <- bytes.Clone(data)
	} else {
		q.dropped.Add(1)
	}
	return len(data), nil
}

func (q *diagnosticQueue) close() {
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

func (q *diagnosticQueue) drain(writer diagnosticWriter, emergency *slog.Logger) {
	defer close(q.done)
	var failures int
	for entry := range q.records {
		if _, err := writer.Write(entry); err != nil {
			failures++
			continue
		}
		if dropped := q.dropped.Swap(0); dropped > 0 || failures > 0 {
			emergency.Warn("diagnostics.records_dropped", "queue_dropped", dropped, "write_failures", failures)
			failures = 0
		}
	}
}

func openDiagnostics(ctx context.Context, indexPath, mode, level string) (context.Context, string, func(), error) {
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
	writer := diagnosticWriter{dir: dir, limit: diagnosticFileLimit}
	queue := &diagnosticQueue{records: make(chan []byte, 128), done: make(chan struct{})}
	fields := []any{"version", appVersion, "mode", mode, "pid", os.Getpid(), "session", time.Now().UnixNano()}
	trace := diagnosticTrace{
		logger:    slog.New(slog.NewJSONHandler(queue, &slog.HandlerOptions{Level: threshold})).With(fields...),
		emergency: slog.New(slog.NewJSONHandler(writer, nil)).With(fields...),
	}
	go queue.drain(writer, trace.emergency)
	return context.WithValue(ctx, diagnosticsKey{}, trace), dir, queue.close, nil
}

func diagnosticOperation(ctx context.Context) context.Context {
	trace, ok := ctx.Value(diagnosticsKey{}).(diagnosticTrace)
	if !ok {
		return ctx
	}
	trace.operation = diagnosticSequence.Add(1)
	return context.WithValue(ctx, diagnosticsKey{}, trace)
}

func logDiagnostic(ctx context.Context, level slog.Level, event string, attrs ...slog.Attr) {
	trace, ok := ctx.Value(diagnosticsKey{}).(diagnosticTrace)
	if !ok || !trace.logger.Enabled(ctx, level) {
		return
	}
	if trace.operation != 0 {
		attrs = append(attrs, slog.Uint64("op", trace.operation))
	}
	trace.logger.LogAttrs(ctx, level, event, attrs...)
}

func milliseconds(elapsed time.Duration) float64 { return float64(elapsed.Microseconds()) / 1000 }

func logDuration(ctx context.Context, event string, started time.Time, err error, attrs ...slog.Attr) {
	level, result := slog.LevelInfo, "ok"
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		result = "canceled"
	case errors.Is(err, sql.ErrNoRows):
		result = "empty"
	default:
		level, result = slog.LevelWarn, "error"
		attrs = append(attrs, slog.String("error_kind", diagnosticErrorKind(err)))
	}
	logDiagnostic(ctx, level, event, append(attrs,
		slog.Float64("duration_ms", milliseconds(time.Since(started))), slog.String("result", result))...)
}

type diagnosticCode string

func (code diagnosticCode) Error() string { return string(code) }

// Error strings from the OS, the index and MCP can contain private input.
func diagnosticErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var code diagnosticCode
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
	}
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(err) {
		err = inner
	}
	return fmt.Sprintf("%T", err)
}

func logPanic(ctx context.Context, component string) {
	if failure := recover(); failure != nil {
		if trace, ok := ctx.Value(diagnosticsKey{}).(diagnosticTrace); ok {
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
			trace.emergency.ErrorContext(ctx, "app.panic", "op", trace.operation, "component", component,
				"panic_type", fmt.Sprintf("%T", failure), "stack", stack)
		}
		panic(failure)
	}
}
