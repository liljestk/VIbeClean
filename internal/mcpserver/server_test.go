package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/diag/diagtest"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/service/local"
	"github.com/liljestk/VIbeClean/internal/units"
)

func connect(t *testing.T, ctx context.Context, backend service.Backend) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(backend).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call[T any](t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any) T {
	t.Helper()
	var value T
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError {
		t.Fatalf("%s: %v %+v", name, err, result)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err == nil {
		err = json.Unmarshal(data, &value)
	}
	if err != nil {
		t.Fatalf("%s structured result: %s, %v", name, data, err)
	}
	return value
}

func TestToolsUseTheBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend, err := local.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	session := connect(t, ctx, backend)
	if hello := session.InitializeResult(); hello.ServerInfo.Name != Name || hello.ServerInfo.Version != buildinfo.Version {
		t.Fatalf("unexpected handshake: %+v", hello.ServerInfo)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{ToolCapture, ToolLargest, ToolDrives, ToolListScopes, ToolChanges}) {
		t.Fatalf("tools = %v", names)
	}
	if drives := call[DrivesOutput](t, ctx, session, ToolDrives, struct{}{}); len(drives.Drives) == 0 {
		t.Fatal("expected at least one fixed drive")
	}

	root := t.TempDir()
	diagtest.CreateFile(t, filepath.Join(root, "Data", "big.bin"), 70*units.MiB)
	first := call[ScopeInfo](t, ctx, session, ToolCapture, CaptureInput{Scope: root})
	diagtest.CreateFile(t, filepath.Join(root, "Data", "big.bin"), 75*units.MiB)
	if err := os.Remove(filepath.Join(root, "Data", "big.bin")); err != nil {
		t.Fatal(err)
	}
	diagtest.CreateFile(t, filepath.Join(root, "Data", "big.bin"), 75*units.MiB)
	second := call[ScopeInfo](t, ctx, session, ToolCapture, CaptureInput{Scope: root})
	if first.Scope != second.Scope || second.CaptureID <= first.CaptureID {
		t.Fatalf("captures of one folder should share a scope: %+v %+v", first, second)
	}
	report := call[ChangesOutput](t, ctx, session, ToolChanges, ChangesInput{Scope: root, MinMiB: 1, Series: "large files"})
	if report.Series != model.LargeFiles || report.Baseline == nil || len(report.Changes) != 1 || report.Changes[0].Bytes != 5*units.MiB {
		t.Fatalf("large file growth: %+v", report)
	}
	largest := call[LatestOutput](t, ctx, session, ToolLargest, LatestInput{Scope: second.Scope, Series: model.LargeFiles, Query: "big"})
	if len(largest.Items) != 1 || largest.Items[0].Bytes < 75*units.MiB {
		t.Fatalf("largest files: %+v", largest)
	}
	listed := call[ListOutput](t, ctx, session, ToolListScopes, struct{}{})
	if len(listed.Scopes) != 1 || listed.Scopes[0].CaptureID != second.CaptureID {
		t.Fatalf("scopes: %+v", listed)
	}
	for _, bad := range []ChangesInput{{Direction: "sideways"}, {MinMiB: -1}, {Days: 500}} {
		reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ToolChanges, Arguments: bad})
		if err != nil || reply == nil || !reply.IsError {
			t.Errorf("%+v should fail as a tool result: %v", bad, err)
		}
	}
}
