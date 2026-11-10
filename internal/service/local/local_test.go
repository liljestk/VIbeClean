package local

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/diag/diagtest"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/scan"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

func openService(t *testing.T, ctx context.Context, path string) *Service {
	t.Helper()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeFiles returns a capture of a folder scope with the given readings.
func fakeFiles(t *testing.T, s *Service, drive model.DriveState, readings ...[]model.Record) {
	t.Helper()
	calls := 0
	s.fixedRoots = func() []string { return []string{drive.Root} }
	s.driveStates = func(roots []string) []model.DriveState {
		if len(roots) == 1 && roots[0] == drive.Root {
			return []model.DriveState{drive}
		}
		return nil
	}
	s.dockerDisks = func() []string { return nil }
	s.captureFiles = func(_ context.Context, req scan.Request) (model.Snapshot, error) {
		if calls >= len(readings) {
			t.Fatal("unexpected capture")
		}
		records := readings[calls]
		calls++
		if req.Progress != nil {
			req.Progress(int64(len(records)))
		}
		return model.Snapshot{Scope: model.ScopeKey(req.Scope, []model.DriveState{drive}), Label: req.Scope,
			Captured: time.Now().UnixMilli() + int64(calls),
			Coverage: model.Coverage{ScopeKind: model.ScopeFiles, Checked: int64(len(records)), Subfolders: true, Drives: []model.DriveState{drive}},
			Records:  records}, nil
	}
}

func TestCompareResolvesFiltersAndValidates(t *testing.T) {
	ctx := context.Background()
	s := openService(t, ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	drive := model.DriveState{Root: `C:\`, ID: "c", Total: 100 * units.GiB, Free: 50 * units.GiB}
	folder := func(key string, bytes int64) model.Record {
		return model.Record{Series: model.Folders, Key: key, Name: key, Path: `C:\work\` + key, Bytes: bytes}
	}
	fakeFiles(t, s, drive,
		[]model.Record{folder("grow", units.GiB), folder("shrink", 2*units.GiB), folder("small", units.MiB)},
		[]model.Record{folder("grow", 3*units.GiB), folder("shrink", units.GiB), folder("small", 2*units.MiB)},
	)
	wantProgress := []service.Progress{{FilesChecked: 3}, {FilesChecked: 3, Saving: true}}
	for range 2 {
		var progress []service.Progress
		if _, err := s.Capture(ctx, service.CaptureRequest{Scope: `C:\work`}, func(p service.Progress) { progress = append(progress, p) }); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(progress, wantProgress) {
			t.Fatalf("progress = %+v, want %+v", progress, wantProgress)
		}
	}
	all, err := s.Compare(ctx, service.CompareQuery{Scope: `c:\work\`})
	if err != nil || len(all.Changes) != 3 || all.Baseline.ID == 0 || len(all.Drives) != 1 {
		t.Fatalf("a path should resolve to its scope and list every change: %+v, %v", all, err)
	}
	growth, err := s.Compare(ctx, service.CompareQuery{Scope: all.Latest.Scope, Series: "folders",
		MinimumBytes: model.MinChange, Direction: service.DirectionGrowth, Limit: 5})
	if err != nil || len(growth.Changes) != 1 || growth.Changes[0].Bytes != 2*units.GiB {
		t.Fatalf("growth above the minimum: %+v, %v", growth.Changes, err)
	}
	freed, err := s.Compare(ctx, service.CompareQuery{Scope: all.Latest.Scope, Direction: service.DirectionFreed, Query: "shrink"})
	if err != nil || len(freed.Changes) != 1 || freed.Changes[0].Bytes != -units.GiB {
		t.Fatalf("freed space matching a word: %+v, %v", freed.Changes, err)
	}
	for _, bad := range []service.CompareQuery{{Days: -1}, {Days: service.MaxDays + 1}, {MinimumBytes: -1}, {Limit: -1}, {Direction: 9},
		{Scope: all.Latest.Scope, Series: model.Images}} {
		if _, err := s.Compare(ctx, bad); !errors.Is(err, model.ErrInvalidArgument) {
			t.Errorf("Compare(%+v) = %v, want an invalid argument", bad, err)
		}
	}
	latest, err := s.Latest(ctx, service.LatestQuery{Scope: `C:\work`, Series: model.Folders, Limit: 1})
	if err != nil || len(latest.Snapshot.Records) != 1 || latest.Snapshot.Records[0].Key != "grow" {
		t.Fatalf("latest should sort largest first and apply the limit: %+v, %v", latest, err)
	}
}

func TestResolveScopes(t *testing.T) {
	ctx := context.Background()
	s := openService(t, ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	drive := model.DriveState{Root: `C:\`, ID: "c"}
	fakeFiles(t, s, drive, []model.Record{{Series: model.Folders, Key: "a", Bytes: 1}})
	saved, err := s.Capture(ctx, service.CaptureRequest{Scope: `C:\work`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{
		"":                "files-v1|quick review",
		"quick REVIEW":    "files-v1|quick review",
		saved.Scope:       saved.Scope,
		`C:\work`:         saved.Scope,
		`c:/work/`:        saved.Scope,
		`C:\other\folder`: model.ScopeKey(`C:\other\folder`, []model.DriveState{drive}),
	} {
		if got, err := s.resolve(ctx, ref); err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	// A disconnected drive falls back to the folder's newest saved history.
	s.driveStates = func([]string) []model.DriveState { return nil }
	if got, err := s.resolve(ctx, `C:\work`); err != nil || got != saved.Scope {
		t.Errorf("disconnected drive resolved to %q, %v", got, err)
	}
	for _, ref := range []string{"Docker", "no such scope"} {
		if _, err := s.resolve(ctx, ref); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("resolve(%q) = %v, want not found", ref, err)
		}
	}
	if _, err := s.Latest(ctx, service.LatestQuery{Scope: `D:\never`}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("a location without captures should be not found: %v", err)
	}
}

func TestCaptureRunsOneAtATime(t *testing.T) {
	ctx := context.Background()
	s := openService(t, ctx, filepath.Join(t.TempDir(), "index.sqlite3"))
	s.gate <- struct{}{}
	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := s.Capture(waiting, service.CaptureRequest{Scope: model.DockerScope}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second capture should wait for the first: %v", err)
	}
}

func TestCanceledCaptureLogsWorkAndPreservesHistory(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index.sqlite3")
	ctx, dir, closeLog, err := diag.Open(context.Background(), indexPath, "test", "info")
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	s := openService(t, ctx, indexPath)
	folder := filepath.Join(root, diagtest.PrivateMarker)
	diagtest.CreateFile(t, filepath.Join(folder, "project", "file.bin"), 4*units.MiB)
	baseline, err := s.Capture(diag.Operation(ctx), service.CaptureRequest{Scope: folder}, nil)
	if err != nil || baseline.ID == 0 {
		t.Fatalf("could not create the baseline: id=%d, err=%v", baseline.ID, err)
	}
	captureCtx, cancel := context.WithCancel(diag.Operation(ctx))
	defer cancel()
	canceled, err := s.Capture(captureCtx, service.CaptureRequest{Scope: folder}, func(service.Progress) { cancel() })
	if !errors.Is(err, context.Canceled) || canceled.ID != 0 {
		t.Fatalf("canceled capture returned id=%d, err=%v", canceled.ID, err)
	}
	diff, err := s.Compare(ctx, service.CompareQuery{Scope: baseline.Scope})
	if err != nil || diff.Latest.ID != baseline.ID || diff.Baseline.ID != 0 {
		t.Fatalf("cancellation changed saved history: %+v, %v", diff, err)
	}
	closeLog()
	events, raw := diagtest.ReadEvents(t, dir)
	if strings.Contains(raw, diagtest.PrivateMarker) {
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
