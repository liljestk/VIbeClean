package rpc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// streamingBackend reports progress and then fails or waits, for stream tests.
type streamingBackend struct {
	service.Backend
	fail error
}

func (b streamingBackend) Capture(ctx context.Context, _ service.CaptureRequest, progress func(service.Progress)) (model.Snapshot, error) {
	for checked := int64(1); checked <= 3; checked++ {
		progress(service.Progress{FilesChecked: checked})
	}
	if b.fail != nil {
		return model.Snapshot{}, b.fail
	}
	<-ctx.Done()
	return model.Snapshot{}, ctx.Err()
}

func TestCaptureStreamsProgressAndCancels(t *testing.T) {
	remote, _ := serve(t, streamingBackend{fail: model.Conflict("a newer capture finished first")})
	var seen []int64
	_, err := remote.Capture(context.Background(), service.CaptureRequest{}, func(p service.Progress) { seen = append(seen, p.FilesChecked) })
	if !errors.Is(err, model.ErrConflict) || err.Error() != "a newer capture finished first" || len(seen) != 3 {
		t.Fatalf("progress %v, err %v", seen, err)
	}

	waiting, _ := serve(t, streamingBackend{})
	ctx, cancel := context.WithCancel(context.Background())
	_, err = waiting.Capture(ctx, service.CaptureRequest{}, func(p service.Progress) {
		if p.FilesChecked == 3 {
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
