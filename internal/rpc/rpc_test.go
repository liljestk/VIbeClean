package rpc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/liljestk/VIbeClean/gen/spacereview/v1"
	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/diag/diagtest"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/service/local"
	"github.com/liljestk/VIbeClean/internal/units"
)

const testToken = "test-token"

// serve starts an in-memory server and returns a client and a raw connection.
func serve(t *testing.T, backend service.Backend) (*Client, *grpc.ClientConn) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	srv := NewGRPCServer(context.Background(), backend, testToken)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(srv.Stop)
	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })
	client, err := Dial("127.0.0.1:0", testToken, dialer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	raw, err := grpc.NewClient("passthrough:///bufnet", dialer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return client, raw
}

func TestRemoteBackendMatchesLocal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	local, err := local.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	remote, _ := serve(t, local)

	folder := t.TempDir()
	diagtest.CreateFile(t, filepath.Join(folder, "Project", "big.bin"), 80*units.MiB)
	first, err := remote.Capture(ctx, service.CaptureRequest{Scope: folder}, nil)
	if err != nil || first.ID == 0 || first.Coverage.ScopeKind != model.ScopeFiles {
		t.Fatalf("remote capture: %+v, %v", first, err)
	}
	if err := os.Remove(filepath.Join(folder, "Project", "big.bin")); err != nil {
		t.Fatal(err)
	}
	second, err := remote.Capture(ctx, service.CaptureRequest{Scope: folder}, nil)
	if err != nil {
		t.Fatal(err)
	}

	info, err := remote.Info(ctx)
	if err != nil || info.Version != buildinfo.Version || !strings.HasSuffix(info.IndexPath, "index.sqlite3") {
		t.Fatalf("info: %+v, %v", info, err)
	}
	if drives, err := remote.Drives(ctx); err != nil || len(drives) == 0 {
		t.Fatalf("drives: %+v, %v", drives, err)
	}
	scopes, err := remote.Scopes(ctx)
	if err != nil || len(scopes) != 1 || scopes[0].ID != second.ID || len(scopes[0].Records) != 0 {
		t.Fatalf("scopes: %+v, %v", scopes, err)
	}
	query := service.CompareQuery{Scope: folder, MinimumBytes: units.MiB, Direction: service.DirectionFreed}
	want, err := local.Compare(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Compare(ctx, query)
	if err != nil || got.Latest.ID != want.Latest.ID || got.Baseline.ID != first.ID || len(got.Changes) != len(want.Changes) ||
		len(got.Changes) == 0 || got.Changes[0].Bytes != want.Changes[0].Bytes || got.Changes[0].Before == nil ||
		got.Changes[0].Before.Path != want.Changes[0].Before.Path || len(got.Drives) != len(want.Drives) {
		t.Fatalf("remote comparison differs from local:\n got %+v\nwant %+v\n%v", got, want, err)
	}
	latest, err := remote.Latest(ctx, service.LatestQuery{Scope: folder, Series: "folders"})
	if err != nil || latest.Snapshot.ID != second.ID || latest.Snapshot.Captured != second.Captured {
		t.Fatalf("latest: %+v, %v", latest, err)
	}
}

func TestErrorsKeepTheirKind(t *testing.T) {
	ctx := context.Background()
	local, err := local.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	remote, _ := serve(t, local)
	if _, err := remote.Latest(ctx, service.LatestQuery{Scope: "no such scope"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing scope: %v", err)
	}
	if _, err := remote.Compare(ctx, service.CompareQuery{Days: -1}); !errors.Is(err, model.ErrInvalidArgument) {
		t.Fatalf("invalid window: %v", err)
	}
	if _, err := remote.Latest(ctx, service.LatestQuery{Series: "nonsense"}); !errors.Is(err, model.ErrInvalidArgument) {
		t.Fatalf("unknown series: %v", err)
	}
	if _, err := remote.Capture(ctx, service.CaptureRequest{Scope: "relative"}, nil); !errors.Is(err, model.ErrInvalidArgument) {
		t.Fatalf("relative capture path: %v", err)
	}
}

func TestDetailedCaptureAndMatchingExclusions(t *testing.T) {
	ctx := context.Background()
	backend, err := local.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	remote, _ := serve(t, backend)
	root := t.TempDir()
	data, skip := filepath.Join(root, "Data"), filepath.Join(root, "Ignored")
	zip, copyPath := filepath.Join(data, "medium.zip"), filepath.Join(data, "copy.zip")
	diagtest.CreateFile(t, zip, 4*units.MiB)
	diagtest.CreateFile(t, copyPath, 4*units.MiB)
	if err := os.Link(zip, filepath.Join(data, "alias.zip")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(data, "different.txt")
	diagtest.CreateFile(t, other, 4*units.MiB)
	f, err := os.OpenFile(other, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("different"))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	diagtest.CreateFile(t, filepath.Join(skip, "large.zip"), 6*units.MiB)
	first, err := remote.Capture(ctx, service.CaptureRequest{Scope: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := service.CaptureRequest{Scope: root, Detail: true, Exclusions: []string{skip}}
	second, err := remote.Capture(ctx, req, nil)
	if err != nil || !second.Coverage.Inventory || len(second.Coverage.Exclusions) != 1 || second.Coverage.Checked != 4 {
		t.Fatalf("detailed capture = %+v, %v", second, err)
	}
	q := service.InventoryQuery{CaptureID: second.ID, Files: true, Extension: ".ZIP", Within: data, SortColumn: 1, Limit: 1}
	page, err := remote.Inventory(ctx, q)
	if err != nil || page.Total != 3 || len(page.Items) != 1 || filepath.Ext(page.Items[0].Path) != ".zip" ||
		page.Items[0].Bytes <= 0 || page.Items[0].Bytes >= model.FileFloor {
		t.Fatalf("small file inventory = %+v, %v", page, err)
	}
	var streamed []model.Record
	if err := remote.StreamInventory(ctx, q, func(row model.Record) error {
		streamed = append(streamed, row)
		return nil
	}); err != nil || len(streamed) != page.Total || streamed[0].Key != page.Items[0].Key {
		t.Fatalf("filtered stream = %+v, %v", streamed, err)
	}
	children, err := remote.Inventory(ctx, service.InventoryQuery{CaptureID: second.ID, Limit: 8})
	if err != nil || len(children.Items) != 1 || children.Items[0].Path != data || children.Items[0].Files != 4 {
		t.Fatalf("root inventory = %+v, %v", children, err)
	}
	dupes, err := remote.Duplicates(ctx, second.ID, data)
	if err != nil || len(dupes.Groups) != 1 || dupes.Checked != 4 || dupes.Skipped != 0 {
		t.Fatalf("duplicates = %+v, %v", dupes, err)
	}
	group := dupes.Groups[0]
	if len(group.Copies) != 2 || len(group.Copies[0].Paths)+len(group.Copies[1].Paths) != 3 ||
		group.Reclaimable(group.SuggestedKeep()) != page.Items[0].Bytes {
		t.Fatalf("duplicate copies or reclaimable size = %+v", group)
	}
	if _, err := remote.Compare(ctx, service.CompareQuery{Scope: root, FromID: first.ID, ToID: second.ID}); !errors.Is(err, model.ErrInvalidArgument) {
		t.Fatalf("comparison of different exclusions = %v", err)
	}
	third, err := remote.Capture(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Inventory(ctx, q); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("old inventory is still current: %v", err)
	}
	history, err := remote.History(ctx, root)
	if err != nil || len(history) != 3 || history[0].ID != third.ID || history[2].ID != first.ID {
		t.Fatalf("history = %+v, %v", history, err)
	}
	diff, err := remote.Compare(ctx, service.CompareQuery{Scope: root, FromID: second.ID, ToID: third.ID})
	if err != nil || diff.Baseline.ID != second.ID || diff.Latest.ID != third.ID || len(diff.Changes) != 0 {
		t.Fatalf("exact comparison = %+v, %v", diff, err)
	}
}

// streamingBackend reports progress and then fails or waits, for stream tests.
type streamingBackend struct {
	service.Backend
	fail error
}

func (b streamingBackend) Capture(ctx context.Context, _ service.CaptureRequest, progress func(service.Progress)) (model.Snapshot, error) {
	for checked := int64(1); checked <= 3; checked++ {
		progress(service.Progress{FilesChecked: checked})
	}
	progress(service.Progress{FilesChecked: 3, Saving: true})
	if b.fail != nil {
		return model.Snapshot{}, b.fail
	}
	<-ctx.Done()
	return model.Snapshot{}, ctx.Err()
}

func TestCaptureStreamsProgressAndCancels(t *testing.T) {
	remote, _ := serve(t, streamingBackend{fail: model.Conflict("a newer capture finished first")})
	var seen []service.Progress
	_, err := remote.Capture(context.Background(), service.CaptureRequest{}, func(p service.Progress) { seen = append(seen, p) })
	want := []service.Progress{{FilesChecked: 1}, {FilesChecked: 2}, {FilesChecked: 3}, {FilesChecked: 3, Saving: true}}
	if !errors.Is(err, model.ErrConflict) || err.Error() != "a newer capture finished first" || !slices.Equal(seen, want) {
		t.Fatalf("progress %v, err %v", seen, err)
	}

	waiting, _ := serve(t, streamingBackend{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = waiting.Capture(ctx, service.CaptureRequest{}, func(p service.Progress) {
		if p.Saving {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled call should cancel the capture: %v", err)
	}
}

func TestAuthentication(t *testing.T) {
	local, err := local.Open(context.Background(), filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	_, raw := serve(t, local)
	health, err := healthpb.NewHealthClient(raw).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || health.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health checks should not need the token: %v, %v", health, err)
	}
	anonymous := &Client{conn: raw, api: pb.NewSpaceReviewServiceClient(raw)}
	if _, err := anonymous.Scopes(context.Background()); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("a call without the token should be refused: %v", err)
	}
}

func TestLoopbackAndTokenFile(t *testing.T) {
	for _, address := range []string{"127.0.0.1:1", "localhost:1", "[::1]:1"} {
		if err := checkLoopback(address); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:1", "192.168.1.2:1", ":1"} {
		if err := checkLoopback(address); err == nil {
			t.Errorf("%s should be refused", address)
		}
	}
	if err := Serve(context.Background(), nil, "0.0.0.0:0", testToken, nil); err == nil {
		t.Fatal("Serve should refuse a public address")
	}
	path := DefaultTokenFile(filepath.Join(t.TempDir(), "nested", "index.sqlite3"))
	first, err := LoadOrCreateToken(path)
	if err != nil || len(first) != 2*tokenBytes {
		t.Fatalf("new token %q: %v", first, err)
	}
	again, err := LoadOrCreateToken(path)
	if err != nil || again != first {
		t.Fatalf("the token should be reused: %q, %v", again, err)
	}
}

func TestLegacyTokenIsRotatedBeforeUse(t *testing.T) {
	path := DefaultTokenFile(filepath.Join(t.TempDir(), "index.sqlite3"))
	legacy := strings.Repeat("a", 2*tokenBytes)
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	// An elevated process can default new files to the Administrators group.
	// Exercise migration of an inherited ACL on a file owned by this user.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION,
		user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(path); !errors.Is(err, errUnprotectedToken) {
		t.Fatalf("clients must refuse an inherited ACL: %v", err)
	}
	rotated, err := LoadOrCreateToken(path)
	if err != nil || rotated == legacy || len(rotated) != len(legacy) {
		t.Fatalf("could not rotate legacy token: %v", err)
	}
	if token, err := ReadToken(path); err != nil || token != rotated {
		t.Fatalf("could not read rotated token: %v", err)
	}
}

func TestServeStopsWithContext(t *testing.T) {
	local, err := local.Open(context.Background(), filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() { done <- Serve(ctx, local, "127.0.0.1:0", testToken, func(addr net.Addr) { ready <- addr }) }()
	address := (<-ready).String()
	client, err := Dial(address, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if info, err := client.Info(context.Background()); err != nil || info.Version == "" {
		t.Fatalf("info over TCP: %+v, %v", info, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if _, err := client.Info(context.Background()); !errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("a stopped server should be unavailable: %v", err)
	}
}
