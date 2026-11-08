package local

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/docker"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/scan"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/store"
	"github.com/liljestk/VIbeClean/internal/units"
	"github.com/liljestk/VIbeClean/internal/volume"
)

// largeCapture is the file count after which a capture returns memory to Windows.
const largeCapture = 500_000

// Service is the in-process backend over a local index.
type Service struct {
	index     *store.Index
	indexPath string
	// gate allows one capture at a time in this process. Captures from other
	// processes are ordered by the index.
	gate chan struct{}

	// Measurement and drive probes, replaceable in tests.
	captureFiles  func(context.Context, scan.Request) (model.Snapshot, error)
	captureDocker func(context.Context) (model.Snapshot, error)
	dockerDisks   func() []string
	fixedRoots    func() []string
	driveStates   func([]string) []model.DriveState
}

var _ service.Backend = (*Service)(nil)

// New returns a backend over an open index. It takes ownership of the index.
func New(index *store.Index, indexPath string) *Service {
	return &Service{
		index:         index,
		indexPath:     indexPath,
		gate:          make(chan struct{}, 1),
		captureFiles:  scan.Capture,
		captureDocker: docker.Capture,
		dockerDisks:   docker.Disks,
		fixedRoots:    volume.FixedRoots,
		driveStates:   volume.States,
	}
}

// Open opens the index at path and returns a backend over it.
func Open(ctx context.Context, path string) (*Service, error) {
	index, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return New(index, path), nil
}

// Close closes the index.
func (s *Service) Close() error { return s.index.Close() }

// Info identifies the backend.
func (s *Service) Info(context.Context) (service.Info, error) {
	return service.Info{Version: buildinfo.Version, IndexPath: s.indexPath}, nil
}

// Drives returns current readings of the local fixed drives.
func (s *Service) Drives(ctx context.Context) ([]model.DriveState, error) {
	return s.driveStates(s.fixedRoots()), ctx.Err()
}

// Scopes returns the latest capture of every saved scope.
func (s *Service) Scopes(ctx context.Context) ([]model.Snapshot, error) {
	return s.index.List(ctx)
}

// Latest returns the latest capture of a scope with its records filtered and
// sorted largest first.
func (s *Service) Latest(ctx context.Context, q service.LatestQuery) (service.Latest, error) {
	if q.Limit < 0 {
		return service.Latest{}, model.InvalidArgument("limit cannot be negative")
	}
	scope, err := s.resolve(ctx, q.Scope)
	if err != nil {
		return service.Latest{}, err
	}
	saved, err := s.index.Latest(ctx, scope)
	if err != nil {
		return service.Latest{}, err
	}
	series := ""
	if q.Series != "" {
		if series, err = model.ChooseSeries(saved.Coverage.ScopeKind, q.Series, false); err != nil {
			return service.Latest{}, err
		}
	}
	filter := model.ParseFilter(q.Query)
	saved.Records = slices.DeleteFunc(saved.Records, func(item model.Record) bool {
		return (series != "" && item.Series != series) || !filter.Matches(item, item.Detail)
	})
	model.SortRecords(saved.Records)
	if q.Limit > 0 && len(saved.Records) > q.Limit {
		saved.Records = saved.Records[:q.Limit]
	}
	return service.Latest{Snapshot: saved, RefreshRecommended: s.refreshRecommended(saved)}, nil
}

// refreshRecommended compares a file capture with the drives it covered.
func (s *Service) refreshRecommended(saved model.Snapshot) bool {
	if saved.Coverage.ScopeKind != model.ScopeFiles {
		return false
	}
	roots := s.fixedRoots()
	if saved.Label != model.QuickScope {
		roots = []string{model.VolumeRoot(saved.Label)}
	}
	return scan.RefreshRecommended(saved, s.driveStates(roots))
}

// Compare ranks changes between the latest capture of a scope and a baseline.
func (s *Service) Compare(ctx context.Context, q service.CompareQuery) (service.Comparison, error) {
	if err := validate(q); err != nil {
		return service.Comparison{}, err
	}
	scope, err := s.resolve(ctx, q.Scope)
	if err != nil {
		return service.Comparison{}, err
	}
	diff, err := s.index.Compare(ctx, scope, q.Days)
	if err != nil {
		return service.Comparison{}, err
	}
	series := ""
	if q.Series != "" {
		if series, err = model.ChooseSeries(diff.Latest.Coverage.ScopeKind, q.Series, true); err != nil {
			return service.Comparison{}, err
		}
	}
	result := service.Comparison{Comparison: diff}
	if diff.Baseline.ID != 0 {
		result.Drives = model.DriveChanges(diff.Baseline.Coverage.Drives, diff.Latest.Coverage.Drives)
	}
	filter := model.ParseFilter(q.Query)
	result.Changes = slices.DeleteFunc(diff.Changes, func(item model.Change) bool {
		return !keep(item, series, q, filter)
	})
	if q.Limit > 0 && len(result.Changes) > q.Limit {
		result.Changes = result.Changes[:q.Limit]
	}
	return result, nil
}

func validate(q service.CompareQuery) error {
	switch {
	case q.Days < 0 || q.Days > service.MaxDays:
		return model.InvalidArgument("choose a window from 0 to %d days", service.MaxDays)
	case q.MinimumBytes < 0:
		return model.InvalidArgument("the minimum change cannot be negative")
	case q.Limit < 0:
		return model.InvalidArgument("limit cannot be negative")
	case q.Direction < service.DirectionBoth || q.Direction > service.DirectionFreed:
		return model.InvalidArgument("direction must be both, growth or freed")
	}
	return nil
}

func keep(item model.Change, series string, q service.CompareQuery, filter model.Filter) bool {
	return (series == "" || item.Series == series) &&
		units.Magnitude(item.Bytes) >= q.MinimumBytes &&
		(q.Direction != service.DirectionGrowth || item.Bytes > 0) &&
		(q.Direction != service.DirectionFreed || item.Bytes < 0) &&
		filter.Matches(item.Item(), item.Item().Detail)
}

// Capture measures a scope and saves it. One capture runs at a time; later
// requests wait for it or for their context to end.
func (s *Service) Capture(ctx context.Context, req service.CaptureRequest, progress func(service.Progress)) (saved model.Snapshot, err error) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return model.Snapshot{}, ctx.Err()
	}
	started := time.Now()
	scopeType := model.ScopeType(req.Scope)
	stage := "measure"
	diag.Log(ctx, slog.LevelInfo, "capture.start", slog.String("scope_type", scopeType))
	defer func() {
		diag.Duration(ctx, "capture.finish", started, err, slog.String("scope_type", scopeType), slog.String("stage", stage),
			slog.Int64("capture_id", saved.ID), slog.Int64("files_checked", saved.Coverage.Checked))
	}()
	saved, err = s.measure(ctx, req, progress)
	if err == nil {
		stage = "save"
		err = s.index.Save(ctx, &saved)
	}
	if saved.Coverage.Checked >= largeCapture {
		releaseStarted := time.Now()
		debug.FreeOSMemory()
		diag.Duration(ctx, "capture.memory_release", releaseStarted, nil)
	}
	if err != nil {
		return model.Snapshot{}, err
	}
	return saved, nil
}

func (s *Service) measure(ctx context.Context, req service.CaptureRequest, progress func(service.Progress)) (model.Snapshot, error) {
	if strings.EqualFold(strings.TrimSpace(req.Scope), model.DockerScope) {
		return s.captureDocker(ctx)
	}
	request := scan.Request{Scope: strings.TrimSpace(req.Scope), Kind: req.Kind}
	if request.Scope == "" || strings.EqualFold(request.Scope, model.QuickScope) {
		request.Scope = model.QuickScope
		request.Extra = s.dockerDisks()
	}
	if progress != nil {
		request.Progress = func(checked int64) { progress(service.Progress{FilesChecked: checked}) }
	}
	return s.captureFiles(ctx, request)
}

// resolve turns a scope ID, a label or an absolute folder into a scope ID.
func (s *Service) resolve(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, model.QuickScope) {
		return model.ScopeKey(model.QuickScope, nil), nil
	}
	saved, err := s.index.List(ctx)
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(saved, func(item model.Snapshot) bool { return item.Scope == ref }) {
		return ref, nil
	}
	if strings.EqualFold(ref, model.DockerScope) {
		for _, item := range saved {
			if item.Coverage.ScopeKind == model.ScopeDocker {
				return item.Scope, nil
			}
		}
		return "", model.NotFound("no saved Docker capture. Refresh Docker first")
	}
	if path, ok := location(ref); ok {
		return s.locationScope(path, saved), nil
	}
	for _, item := range saved {
		if strings.EqualFold(item.Label, ref) {
			return item.Scope, nil
		}
	}
	return "", model.NotFound("no saved capture for %q. Capture that scope first", ref)
}

// location cleans an absolute path on a drive letter, accepting "C:" for "C:\".
func location(ref string) (string, bool) {
	if len(ref) == 2 && ref[1] == ':' {
		ref += `\`
	}
	if !filepath.IsAbs(ref) || model.VolumeRoot(ref) == "" {
		return "", false
	}
	return filepath.Clean(ref), true
}

// locationScope keys a folder by the volume now at its drive letter. When the
// drive is not connected, the newest saved history of that folder is used.
func (s *Service) locationScope(path string, saved []model.Snapshot) string {
	if states := s.driveStates([]string{model.VolumeRoot(path)}); len(states) > 0 {
		return model.ScopeKey(path, states)
	}
	for _, item := range saved {
		if item.Coverage.ScopeKind == model.ScopeFiles && strings.EqualFold(item.Label, path) {
			return item.Scope
		}
	}
	return model.ScopeKey(path, nil)
}
