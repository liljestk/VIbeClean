package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/liljestk/VIbeClean/internal/diag/diagtest"
	"github.com/liljestk/VIbeClean/internal/mcpserver"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/rpc"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

const mib = units.MiB

// built is the executable under test: SPACE_REVIEW_EXE, or a fresh build.
var built string

func TestMain(m *testing.M) {
	code := func() int {
		if source := os.Getenv("SPACE_REVIEW_EXE"); source != "" {
			built = source
			return m.Run()
		}
		dir, err := os.MkdirTemp("", "space-review-e2e")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		built = filepath.Join(dir, "Space Review.exe")
		build := exec.Command("go", "build", "-ldflags=-H windowsgui", "-o", built, ".")
		if output, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build the executable: %v\n%s", err, output)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// isolatedExecutable copies the executable so each test has its own folder.
func isolatedExecutable(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Space Review.exe")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func version(t *testing.T, exe string) string {
	t.Helper()
	output, err := exec.Command(exe, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func connectMCP(t *testing.T, ctx context.Context, exe string, args ...string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.CommandContext(ctx, exe, append([]string{"--mcp"}, args...)...)
	cmd.Dir = filepath.Dir(exe)
	client := mcp.NewClient(&mcp.Implementation{Name: "space-review-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if hello := session.InitializeResult(); hello.ServerInfo.Name != mcpserver.Name || hello.ServerInfo.Version != version(t, exe) {
		t.Fatalf("unexpected MCP handshake: %+v", hello.ServerInfo)
	}
	return session
}

func callTool[T any](t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any) T {
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

func TestPortableMCPPersistsRealChanges(t *testing.T) {
	exe := isolatedExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "portable.sqlite3")
	root := t.TempDir()
	growing := filepath.Join(root, "Project A", "Cache", "artifact.bin")
	shrinking := filepath.Join(root, "Project B", "old.bin")
	diagtest.CreateFile(t, growing, 72*mib)
	diagtest.CreateFile(t, shrinking, 8*mib)

	first := connectMCP(t, ctx, exe, "--index", path)
	tools, err := first.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"capture_storage", "largest_storage_items", "list_drives", "list_storage_scopes", "storage_changes"}) {
		t.Fatalf("tools = %v", names)
	}
	baseline := callTool[mcpserver.ScopeInfo](t, ctx, first, "capture_storage", mcpserver.CaptureInput{Scope: root})
	if !baseline.Subfolders || baseline.FilesChecked != 2 {
		t.Fatalf("unexpected first capture: %+v", baseline)
	}
	only := callTool[mcpserver.ChangesOutput](t, ctx, first, "storage_changes", mcpserver.ChangesInput{Scope: baseline.Scope})
	if only.Baseline != nil || len(only.Changes) != 0 {
		t.Fatalf("first capture should establish a baseline: %+v", only)
	}

	replacement := filepath.Join(filepath.Dir(growing), "replacement.bin")
	diagtest.CreateFile(t, replacement, 84*mib)
	if err := os.Rename(replacement, growing); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shrinking); err != nil {
		t.Fatal(err)
	}
	diagtest.CreateFile(t, filepath.Join(root, "Noise", "small.bin"), mib/2)
	fresh := callTool[mcpserver.ScopeInfo](t, ctx, first, "capture_storage", mcpserver.CaptureInput{Scope: root})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := connectMCP(t, ctx, exe, "--index", path)
	saved := callTool[mcpserver.ListOutput](t, ctx, reopened, "list_storage_scopes", struct{}{})
	if len(saved.Scopes) != 1 || saved.Scopes[0].CaptureID != fresh.CaptureID {
		t.Fatalf("latest capture did not survive restart: %+v", saved)
	}
	quiet := callTool[mcpserver.ChangesOutput](t, ctx, reopened, "storage_changes", mcpserver.ChangesInput{Scope: fresh.Scope})
	if len(quiet.Changes) != 0 || quiet.Baseline == nil || quiet.Baseline.CaptureID != baseline.CaptureID {
		t.Fatalf("default threshold or previous capture mismatch: %+v", quiet)
	}
	diff := callTool[mcpserver.ChangesOutput](t, ctx, reopened, "storage_changes", mcpserver.ChangesInput{Scope: root, MinMiB: 1})
	if len(diff.Changes) != 2 || diff.Changes[0].Bytes != 12*mib || diff.Changes[1].Bytes != -8*mib ||
		len(diff.Drives) != 1 || diff.ShortWindow {
		t.Fatalf("unexpected folder deltas: %+v", diff)
	}
	for _, series := range []string{model.Subfolders, model.LargeFiles} {
		report := callTool[mcpserver.ChangesOutput](t, ctx, reopened, "storage_changes",
			mcpserver.ChangesInput{Scope: fresh.Scope, MinMiB: 1, Days: 7, Series: series,
				Query: "path:" + filepath.Join(root, "Project A"), Direction: "growth"})
		if !report.ShortWindow || len(report.Changes) != 1 || report.Changes[0].Bytes != 12*mib || report.Changes[0].Bounded {
			t.Fatalf("unexpected %s changes: %+v", series, report)
		}
		if series == model.LargeFiles && report.Changes[0].Before.Key == report.Changes[0].After.Key {
			t.Fatal("the test should replace the file with a new Windows identity")
		}
	}
	month := callTool[mcpserver.ChangesOutput](t, ctx, reopened, "storage_changes",
		mcpserver.ChangesInput{Scope: fresh.Scope, MinMiB: 1, Days: 30, Direction: "freed"})
	if !month.ShortWindow || len(month.Changes) != 1 || month.Changes[0].Bytes != -8*mib {
		t.Fatalf("unexpected monthly window: %+v", month)
	}
	largest := callTool[mcpserver.LatestOutput](t, ctx, reopened, "largest_storage_items",
		mcpserver.LatestInput{Scope: fresh.Scope, Series: model.LargeFiles, Query: "artifact"})
	if len(largest.Items) != 1 || largest.Items[0].Bytes != 84*mib {
		t.Fatalf("unexpected latest files: %+v", largest)
	}
	rejected, err := reopened.CallTool(ctx, &mcp.CallToolParams{Name: "largest_storage_items",
		Arguments: mcpserver.LatestInput{Scope: diagtest.PrivateMarker, Query: diagtest.PrivateMarker}})
	if err != nil || rejected == nil || !rejected.IsError {
		t.Fatalf("unknown scope should fail as a tool result: %v", err)
	}
	unknown, err := reopened.CallTool(ctx, &mcp.CallToolParams{Name: diagtest.PrivateMarker, Arguments: struct{}{}})
	if err == nil && (unknown == nil || !unknown.IsError) {
		t.Fatal("an unregistered tool should fail")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	checkMCPTrace(t, filepath.Join(filepath.Dir(path), "diagnostics"), fresh)
}

func checkMCPTrace(t *testing.T, dir string, fresh mcpserver.ScopeInfo) {
	t.Helper()
	events, raw := diagtest.ReadEvents(t, dir)
	if strings.Contains(raw, diagtest.PrivateMarker) || strings.Contains(raw, "artifact.bin") || strings.Contains(raw, "portable.sqlite3") {
		t.Fatal("the MCP log contained private input")
	}
	var capture diagtest.Event
	sessions := map[int64]bool{}
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
			args := []string{"--index", path}
			if test == "off" {
				args = append(args, "--log-level", "off")
			} else if err := os.WriteFile(dir, []byte("occupied"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			session := connectMCP(t, ctx, exe, args...)
			fixture := t.TempDir()
			diagtest.CreateFile(t, filepath.Join(fixture, "data.bin"), 4*mib)
			saved := callTool[mcpserver.ScopeInfo](t, ctx, session, "capture_storage", mcpserver.CaptureInput{Scope: fixture})
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
	sessions := []*mcp.ClientSession{connectMCP(t, ctx, exe, "--index", path), connectMCP(t, ctx, exe, "--index", path)}
	type outcome struct {
		reply *mcp.CallToolResult
		err   error
	}
	start, results := make(chan struct{}), make(chan outcome, len(sessions))
	for _, session := range sessions {
		scope := t.TempDir()
		diagtest.CreateFile(t, filepath.Join(scope, "payload.bin"), 4*mib)
		go func() {
			<-start
			reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "capture_storage", Arguments: mcpserver.CaptureInput{Scope: scope}})
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
	report := callTool[mcpserver.ListOutput](t, ctx, sessions[0], "list_storage_scopes", struct{}{})
	if len(report.Scopes) != 2 || report.Scopes[0].CaptureID == report.Scopes[1].CaptureID {
		t.Fatalf("expected two committed captures: %+v", report)
	}
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := diagtest.ReadEvents(t, filepath.Join(root, "diagnostics"))
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

// TestAPIServerSharesHistoryWithMCP runs the executable as an API server and
// checks that a gRPC client and an MCP client using --connect see the same history.
func TestAPIServerSharesHistoryWithMCP(t *testing.T) {
	exe := isolatedExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	base := t.TempDir()
	index := filepath.Join(base, "server.sqlite3")
	server := exec.CommandContext(ctx, exe, "--serve", "--index", index, "--listen", "127.0.0.1:0")
	stdout, err := server.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	})
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() {
		t.Fatalf("the server printed no address: %v", lines.Err())
	}
	address, ok := strings.CutPrefix(lines.Text(), "Space Review API listening on ")
	if !ok {
		t.Fatalf("unexpected server output %q", lines.Text())
	}
	token, err := rpc.ReadToken(rpc.DefaultTokenFile(index))
	if err != nil {
		t.Fatal(err)
	}
	client, err := rpc.Dial(address, token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	folder := t.TempDir()
	diagtest.CreateFile(t, filepath.Join(folder, "Data", "big.bin"), 70*mib)
	var progressed bool
	saved, err := client.Capture(ctx, service.CaptureRequest{Scope: folder}, func(service.Progress) { progressed = true })
	if err != nil || saved.ID == 0 {
		t.Fatalf("capture through the API: %+v, %v", saved, err)
	}
	t.Logf("progress events received: %t", progressed)

	session := connectMCP(t, ctx, exe, "--connect", address, "--token-file", rpc.DefaultTokenFile(index),
		"--index", filepath.Join(base, "client.sqlite3"))
	listed := callTool[mcpserver.ListOutput](t, ctx, session, "list_storage_scopes", struct{}{})
	if len(listed.Scopes) != 1 || listed.Scopes[0].CaptureID != saved.ID {
		t.Fatalf("MCP over --connect should see the server's history: %+v", listed)
	}
	largest := callTool[mcpserver.LatestOutput](t, ctx, session, "largest_storage_items",
		mcpserver.LatestInput{Scope: folder, Series: model.LargeFiles})
	if len(largest.Items) != 1 || largest.Items[0].Bytes < 70*mib {
		t.Fatalf("large files through MCP and the API: %+v", largest)
	}
	if _, err := os.Stat(filepath.Join(base, "client.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("a connected client must not create its own index: %v", err)
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
	session := connectMCP(t, ctx, exe, "--index", filepath.Join(root, "docker.sqlite3"))
	saved := callTool[mcpserver.ScopeInfo](t, ctx, session, "capture_storage", mcpserver.CaptureInput{Scope: model.DockerScope})
	if saved.Kind != model.ScopeDocker || !strings.HasPrefix(saved.Scope, "docker-v1|") || saved.Engine == "" {
		t.Fatalf("unexpected local engine: %+v", saved)
	}
	report := callTool[mcpserver.LatestOutput](t, ctx, session, "largest_storage_items",
		mcpserver.LatestInput{Scope: saved.Scope, Series: model.DockerTotals})
	for _, key := range []string{"host", model.Images, model.Containers, model.Volumes, model.BuildCache} {
		if !slices.ContainsFunc(report.Items, func(row model.Record) bool { return row.Key == key }) {
			t.Fatalf("missing %s in %+v", key, report.Items)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	events, _ := diagtest.ReadEvents(t, filepath.Join(root, "diagnostics"))
	if !slices.ContainsFunc(events, func(row diagtest.Event) bool {
		return row.Message == "docker.api" && row.Endpoint == "/system/df" && row.Status == 200 &&
			row.Duration != nil && *row.Duration > 0 && row.ResponseBytes > 0
	}) {
		t.Fatal("the local Docker usage request has no timing in the log")
	}
	t.Logf("live capture from %s with %d separate usage readings", saved.Engine, len(report.Items))
}
