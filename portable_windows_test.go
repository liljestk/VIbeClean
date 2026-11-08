package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func isolatedExecutable(t *testing.T) string {
	t.Helper()
	source := os.Getenv("SPACE_REVIEW_EXE")
	if source == "" {
		t.Skip("set SPACE_REVIEW_EXE to test the built Windows executable")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Space Review.exe")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func connectPortable(t *testing.T, ctx context.Context, exe, indexPath string, options ...string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.CommandContext(ctx, exe, append([]string{"--mcp", "--index", indexPath}, options...)...)
	cmd.Dir = filepath.Dir(exe)
	client := mcp.NewClient(&mcp.Implementation{Name: "space-review-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if hello := session.InitializeResult(); hello.ServerInfo.Name != "space-review" || hello.ServerInfo.Version != appVersion {
		t.Fatalf("unexpected MCP handshake: %+v", hello)
	}
	return session
}

func callPortable[T any](t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any) T {
	t.Helper()
	var value T
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s: %+v", name, result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err == nil {
		err = json.Unmarshal(data, &value)
	}
	if err != nil || result.StructuredContent == nil {
		t.Fatalf("%s structured result: %s, %v", name, data, err)
	}
	return value
}

func createFile(t *testing.T, path string, size int64) {
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

func TestPortableMCPPersistsRealChanges(t *testing.T) {
	exe := isolatedExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "portable.sqlite3")
	root := t.TempDir()
	growing := filepath.Join(root, "Project A", "Cache", "artifact.bin")
	shrinking := filepath.Join(root, "Project B", "old.bin")
	createFile(t, growing, 72*mib)
	createFile(t, shrinking, 8*mib)

	first := connectPortable(t, ctx, exe, path)
	tools, err := first.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"capture_storage", "largest_storage_items", "list_storage_scopes", "storage_changes"}) {
		t.Fatalf("tools = %v", names)
	}
	baseline := callPortable[scopeInfo](t, ctx, first, "capture_storage", captureInput{Scope: root})
	if !baseline.Subfolders || baseline.FilesChecked != 2 {
		t.Fatalf("unexpected first capture: %+v", baseline)
	}
	only := callPortable[changesOutput](t, ctx, first, "storage_changes", changesInput{Scope: baseline.Scope})
	if only.Baseline != nil || len(only.Changes) != 0 {
		t.Fatalf("first capture should establish a baseline: %+v", only)
	}

	replacement := filepath.Join(filepath.Dir(growing), "replacement.bin")
	createFile(t, replacement, 84*mib)
	if err := os.Rename(replacement, growing); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shrinking); err != nil {
		t.Fatal(err)
	}
	createFile(t, filepath.Join(root, "Noise", "small.bin"), mib/2)
	fresh := callPortable[scopeInfo](t, ctx, first, "capture_storage", captureInput{Scope: root})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := connectPortable(t, ctx, exe, path)
	saved := callPortable[listOutput](t, ctx, reopened, "list_storage_scopes", struct{}{})
	if len(saved.Scopes) != 1 || saved.Scopes[0].CaptureID != fresh.CaptureID {
		t.Fatalf("latest capture did not survive restart: %+v", saved)
	}
	quiet := callPortable[changesOutput](t, ctx, reopened, "storage_changes", changesInput{Scope: fresh.Scope})
	if len(quiet.Changes) != 0 || quiet.Baseline == nil || quiet.Baseline.CaptureID != baseline.CaptureID {
		t.Fatalf("default threshold or previous capture mismatch: %+v", quiet)
	}
	diff := callPortable[changesOutput](t, ctx, reopened, "storage_changes", changesInput{Scope: fresh.Scope, MinMiB: 1})
	if len(diff.Changes) != 2 || diff.Changes[0].Bytes != 12*mib || diff.Changes[1].Bytes != -8*mib ||
		len(diff.Drives) != 1 || diff.ShortWindow {
		t.Fatalf("unexpected folder deltas: %+v", diff)
	}
	for _, series := range []string{subfolders, largeFiles} {
		report := callPortable[changesOutput](t, ctx, reopened, "storage_changes",
			changesInput{Scope: fresh.Scope, MinMiB: 1, Days: 7, Series: series,
				Query: "path:" + filepath.Join(root, "Project A"), Direction: "growth"})
		if !report.ShortWindow || len(report.Changes) != 1 || report.Changes[0].Bytes != 12*mib || report.Changes[0].Bounded {
			t.Fatalf("unexpected %s changes: %+v", series, report)
		}
		if series == largeFiles && report.Changes[0].Before.Key == report.Changes[0].After.Key {
			t.Fatal("the test should replace the file with a new Windows identity")
		}
	}
	month := callPortable[changesOutput](t, ctx, reopened, "storage_changes",
		changesInput{Scope: fresh.Scope, MinMiB: 1, Days: 30, Direction: "freed"})
	if !month.ShortWindow || len(month.Changes) != 1 || month.Changes[0].Bytes != -8*mib {
		t.Fatalf("unexpected monthly window: %+v", month)
	}
	largest := callPortable[latestOutput](t, ctx, reopened, "largest_storage_items",
		latestInput{Scope: fresh.Scope, Series: largeFiles, Query: "artifact"})
	if len(largest.Items) != 1 || largest.Items[0].Bytes != 84*mib {
		t.Fatalf("unexpected latest files: %+v", largest)
	}
	rejected, err := reopened.CallTool(ctx, &mcp.CallToolParams{Name: "largest_storage_items",
		Arguments: latestInput{Scope: privateLogMarker, Query: privateLogMarker}})
	if err != nil || rejected == nil || !rejected.IsError {
		t.Fatalf("unknown scope should fail as a tool result: %v", err)
	}
	unknown, err := reopened.CallTool(ctx, &mcp.CallToolParams{Name: privateLogMarker, Arguments: struct{}{}})
	if err == nil && (unknown == nil || !unknown.IsError) {
		t.Fatal("an unregistered tool should fail")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	events, raw := readDiagnosticEvents(t, filepath.Join(filepath.Dir(path), "diagnostics"))
	if strings.Contains(raw, privateLogMarker) || strings.Contains(raw, "artifact.bin") || strings.Contains(raw, "portable.sqlite3") {
		t.Fatal("the MCP log contained private input")
	}
	var capture diagnosticEvent
	var sessions = map[int64]bool{}
	var loggedFailure, loggedUnknown bool
	for _, event := range events {
		sessions[event.Session] = true
		if event.Message == "capture.finish" && event.CaptureID == fresh.CaptureID {
			capture = event
		}
		if event.Message == "mcp.request" && event.Tool == "largest_storage_items" && event.Result == "error" && event.ErrorKind == "tool_error" {
			loggedFailure = true
		}
		if event.Message == "mcp.request" && event.Tool == "unknown" && event.Result == "error" {
			loggedUnknown = true
		}
	}
	if len(sessions) != 2 || !loggedFailure || !loggedUnknown || capture.Operation == 0 || capture.ScopeType != "folder" ||
		capture.FilesChecked != fresh.FilesChecked || capture.Result != "ok" {
		t.Fatalf("incomplete trace across restart, sessions=%d, failure=%t, unknown=%t, capture=%+v",
			len(sessions), loggedFailure, loggedUnknown, capture)
	}
	phases := map[string]bool{}
	for _, event := range events {
		if event.Session == capture.Session && event.Operation == capture.Operation {
			phases[event.Message] = true
		}
	}
	for _, phase := range []string{"capture.start", "scan.files", "index.save", "capture.finish", "mcp.request"} {
		if !phases[phase] {
			t.Fatalf("missing %s in the capture trace: %+v", phase, phases)
		}
	}
}

func TestPortableStorageSurvivesDisabledOrUnavailableLogs(t *testing.T) {
	exe := isolatedExecutable(t)
	for _, test := range []string{"off", "unavailable"} {
		t.Run(test, func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, "index.sqlite3")
			dir := filepath.Join(base, "diagnostics")
			var opts []string
			if test == "off" {
				opts = []string{"--log-level", "off"}
			} else if err := os.WriteFile(dir, []byte("occupied"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			session := connectPortable(t, ctx, exe, path, opts...)
			fixture := t.TempDir()
			createFile(t, filepath.Join(fixture, "data.bin"), 4*mib)
			saved := callPortable[scopeInfo](t, ctx, session, "capture_storage", captureInput{Scope: fixture})
			if saved.CaptureID == 0 || saved.FilesChecked != 1 {
				t.Fatalf("storage capture should succeed without diagnostics: %+v", saved)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if test == "off" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("diagnostics off created output: %v", err)
				}
			} else if value, err := os.ReadFile(dir); err != nil || string(value) != "occupied" {
				t.Fatalf("the existing diagnostics path changed: %v", err)
			}
		})
	}
}

func TestPortableConcurrentCapturesShareHistoryAndLogs(t *testing.T) {
	exe := isolatedExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	path := filepath.Join(root, "shared.sqlite3")
	sessions := []*mcp.ClientSession{connectPortable(t, ctx, exe, path), connectPortable(t, ctx, exe, path)}
	type outcome struct {
		reply *mcp.CallToolResult
		err   error
	}
	start, results := make(chan struct{}), make(chan outcome, len(sessions))
	for _, session := range sessions {
		scope := t.TempDir()
		createFile(t, filepath.Join(scope, "payload.bin"), 4*mib)
		go func() {
			<-start
			reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "capture_storage", Arguments: captureInput{Scope: scope}})
			results <- outcome{reply, err}
		}()
	}
	close(start)
	for range sessions {
		result := <-results
		if result.err != nil || result.reply == nil || result.reply.IsError {
			t.Fatalf("parallel capture failed: %+v %v", result.reply, result.err)
		}
	}
	report := callPortable[listOutput](t, ctx, sessions[0], "list_storage_scopes", struct{}{})
	if len(report.Scopes) != 2 || report.Scopes[0].CaptureID == report.Scopes[1].CaptureID {
		t.Fatalf("expected two committed captures: %+v", report)
	}
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := readDiagnosticEvents(t, filepath.Join(root, "diagnostics"))
	captures := map[int64]bool{}
	for _, entry := range events {
		if entry.Message == "capture.finish" && entry.Result == "ok" {
			captures[entry.Session] = true
		}
	}
	if len(captures) != 2 {
		t.Fatalf("both MCP processes should record the capture in one log: %+v", captures)
	}
}

func TestPortableLiveDocker(t *testing.T) {
	if os.Getenv("SPACE_REVIEW_TEST_LIVE_DOCKER") != "1" {
		t.Skip("set SPACE_REVIEW_TEST_LIVE_DOCKER=1 to query the running local engine")
	}
	exe := isolatedExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	session := connectPortable(t, ctx, exe, filepath.Join(root, "docker.sqlite3"))
	saved := callPortable[scopeInfo](t, ctx, session, "capture_storage", captureInput{Scope: dockerScope})
	if saved.Kind != "docker" || !strings.HasPrefix(saved.Scope, "docker-v1|") || saved.Engine == "" {
		t.Fatalf("unexpected local engine: %+v", saved)
	}
	report := callPortable[latestOutput](t, ctx, session, "largest_storage_items",
		latestInput{Scope: saved.Scope, Series: dockerSum})
	for _, key := range []string{"host", images, containers, volumes, buildCache} {
		if !slices.ContainsFunc(report.Items, func(row record) bool { return row.Key == key }) {
			t.Fatalf("missing %s in %+v", key, report.Items)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	events, _ := readDiagnosticEvents(t, filepath.Join(root, "diagnostics"))
	if !slices.ContainsFunc(events, func(row diagnosticEvent) bool {
		return row.Message == "docker.api" && row.Endpoint == "/system/df" && row.Status == 200 &&
			row.Duration != nil && *row.Duration > 0 && row.ResponseBytes > 0
	}) {
		t.Fatal("the local Docker usage request has no timing in the log")
	}
	t.Logf("live capture from %s with %d separate usage readings", saved.Engine, len(report.Items))
}
