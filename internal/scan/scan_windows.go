// Package scan measures how much space local folders use. It reads Windows
// directory records in batches under normal user permissions, counts hard
// links once, skips junctions and links, and counts only the bytes cloud
// files keep on this disk.
package scan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
	"github.com/liljestk/VIbeClean/internal/volume"
)

var (
	errInvalidOffset = diag.Code("invalid directory record offset")
	errInvalidName   = diag.Code("invalid directory record name")
	errInvalidLength = diag.Code("invalid directory record length")
)

const progressInterval = 200 * time.Millisecond

// Request describes one file capture.
type Request struct {
	// Scope is model.QuickScope, a drive root or an absolute folder.
	Scope string
	// Kind optionally sets the category of a folder scope.
	Kind string
	// Extra lists more files that Quick review measures, such as Docker disks.
	Extra []string
	// Progress, when set, receives the number of files checked so far.
	Progress func(filesChecked int64)
}

func (r *directoryReader) addGap(meta *model.Coverage, path string, err error) {
	if err != nil && !os.IsNotExist(err) && !errors.Is(err, context.Canceled) {
		meta.Gaps = append(meta.Gaps, path)
		if r.gapKinds == nil {
			r.gapKinds = make(map[string]int)
		}
		r.gapKinds[diag.ErrorKind(err)]++
	}
}

type timings struct {
	probe, discover, walk, finalize time.Duration
	targets                         int
}

// Capture measures a scope and returns an unsaved snapshot.
func Capture(ctx context.Context, req Request) (_ model.Snapshot, err error) {
	started := time.Now()
	w := &walker{ctx: ctx, reader: new(directoryReader), progress: req.Progress}
	stage, scopeType := "validate_scope", model.ScopeType(req.Scope)
	var spent timings
	defer func() { w.log(started, err, stage, scopeType, spent) }()

	scope, kind, err := resolve(req.Scope, req.Kind)
	if err != nil {
		return model.Snapshot{}, err
	}
	stage = "drive_probe"
	phaseStarted := time.Now()
	roots := volume.FixedRoots()
	if scope != model.QuickScope {
		roots = []string{model.VolumeRoot(scope)}
	}
	startDisks := volume.States(roots)
	spent.probe = time.Since(phaseStarted)
	if len(startDisks) == 0 {
		return model.Snapshot{}, model.Unavailable("no local fixed drive was available")
	}
	s := &w.snapshot
	s.Scope, s.Label = model.ScopeKey(scope, startDisks), scope
	s.Coverage = model.Coverage{ScopeKind: model.ScopeFiles, Kind: kind, Subfolders: true, Labels: model.LabelsVersion}

	stage = "discover"
	phaseStarted = time.Now()
	targets := []target{{scope, kind, true}}
	if scope == model.QuickScope {
		targets = quickTargets(ctx, w.reader, roots, &s.Coverage, req.Extra)
	}
	spent.targets = len(targets)
	spent.discover = time.Since(phaseStarted)

	stage = "walk"
	phaseStarted = time.Now()
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		w.target(t)
	}
	spent.walk = time.Since(phaseStarted)
	if err := ctx.Err(); err != nil {
		return model.Snapshot{}, err
	}

	stage = "finalize"
	phaseStarted = time.Now()
	s.Coverage.Estimated = w.reader.estimated
	s.Coverage.Drives = volume.States(roots)
	w.addCategories()
	spent.finalize = time.Since(phaseStarted)
	if model.ScopeKey(scope, s.Coverage.Drives) != s.Scope || drivesReplaced(startDisks, s.Coverage.Drives) {
		return model.Snapshot{}, model.Conflict("a drive changed during the scan; scan again for a comparable snapshot")
	}
	slices.Sort(s.Coverage.Gaps)
	s.Coverage.Gaps = slices.Compact(s.Coverage.Gaps)
	s.Captured = time.Now().UnixMilli()
	return *s, nil
}

// resolve validates the scope and picks the category of a folder scope.
func resolve(scope, kind string) (string, string, error) {
	if scope != model.QuickScope {
		path, err := LocalFolder(scope)
		if err != nil {
			return "", "", err
		}
		scope = path
		if kind == "" {
			kind = kindOfPath(path)
		}
	}
	if kind == "" {
		kind = model.FilesKind
	}
	return scope, kind, nil
}

func drivesReplaced(before, after []model.DriveState) bool {
	for _, old := range before {
		for _, now := range after {
			if old.Root == now.Root && old.ID != now.ID {
				return true
			}
		}
	}
	return false
}

// walker accumulates one capture.
type walker struct {
	ctx          context.Context
	reader       *directoryReader
	snapshot     model.Snapshot
	sized        map[fileID]struct{}
	drive        string
	progress     func(int64)
	lastProgress time.Time
	// categories totals the files of each category.
	categories map[string]*model.Record
}

// count adds a file to its category total.
func (w *walker) count(kind string, size int64) {
	total := w.categories[kind]
	if total == nil {
		if w.categories == nil {
			w.categories = make(map[string]*model.Record)
		}
		total = &model.Record{Series: model.Categories, Key: kind, Name: kind, Kind: kind}
		w.categories[kind] = total
	}
	total.Bytes += size
	total.Files++
}

// addCategories adds the category totals to the snapshot.
func (w *walker) addCategories() {
	kinds := make([]string, 0, len(w.categories))
	for kind := range w.categories {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		if total := w.categories[kind]; total.Bytes > 0 {
			w.snapshot.Records = append(w.snapshot.Records, *total)
		}
	}
}

func (w *walker) log(started time.Time, err error, stage, scopeType string, spent timings) {
	c := w.snapshot.Coverage
	diag.Duration(w.ctx, "scan.files", started, err, slog.String("stage", stage), slog.String("scope_type", scopeType),
		slog.Int("targets", spent.targets), slog.Int64("files_checked", c.Checked),
		slog.Int64("directory_attempts", w.reader.directories), slog.Int64("metadata_batches", w.reader.batches),
		slog.Int64("fallback_directories", w.reader.estimated), slog.Int("unreadable", len(c.Gaps)),
		slog.Int64("size_lookups", w.reader.sizeLookups), slog.Int64("size_lookup_failures", w.reader.sizeFailures),
		slog.Any("gap_kinds", w.reader.gapKinds), slog.Int64("repeated_links", c.Shared),
		slog.Int64("skipped_links", c.Links), slog.Int64("online_only", c.Online),
		slog.Int("readings", len(w.snapshot.Records)), slog.Float64("drive_probe_ms", diag.Milliseconds(spent.probe)),
		slog.Float64("discover_ms", diag.Milliseconds(spent.discover)), slog.Float64("walk_ms", diag.Milliseconds(spent.walk)),
		slog.Float64("finalize_ms", diag.Milliseconds(spent.finalize)))
}

func (w *walker) mark() {
	if w.progress != nil && time.Since(w.lastProgress) >= progressInterval {
		w.progress(w.snapshot.Coverage.Checked)
		w.lastProgress = time.Now()
	}
}

// target measures every entry of a split target, or the one entry of another target.
func (w *walker) target(t target) {
	// Hard links are counted once per drive; targets arrive sorted by path.
	if drive := model.VolumeRoot(t.Path); drive != w.drive || w.sized == nil {
		w.sized = make(map[fileID]struct{})
		w.drive = drive
	}
	parent := t.Path
	if !t.Split {
		parent = filepath.Dir(t.Path)
	}
	var entries []dirEntry
	err := w.reader.entries(w.ctx, parent, 0, func(entry dirEntry) {
		if t.Split || strings.EqualFold(entry.Name, filepath.Base(t.Path)) {
			entries = append(entries, entry)
		}
	})
	w.reader.addGap(&w.snapshot.Coverage, parent, err)
	if t.Split {
		// A scope that is itself a project or game labels everything in it.
		if l, ok := contentLabel(entries, t.Kind); ok {
			t.Kind = l.kind
		}
	}
	slices.SortFunc(entries, func(a, b dirEntry) int { return strings.Compare(a.Name, b.Name) })
	for _, entry := range entries {
		if w.ctx.Err() != nil {
			return
		}
		w.entry(t, parent, entry)
	}
}

// entry measures one top-level reading and the readings found inside it.
func (w *walker) entry(t target, parent string, entry dirEntry) {
	if entry.link() {
		w.snapshot.Coverage.Links++
		return
	}
	path := filepath.Join(parent, entry.Name)
	l := label{kind: t.Kind}
	if t.Split {
		l = classify(entry.Name, placeOf(parent, t.Kind), entry.directory())
	}
	bucket := pathRecord(path, l.kind, entry)
	bucket.Detail = l.why
	walked := &tree{w: w, bucket: &bucket}
	root := branch{path: path, lower: model.NormalizedKey(path), kind: l.kind, entry: entry}
	var firstReview *model.Record
	if model.IsCandidate(l.kind) {
		firstReview = &bucket
		root.review = firstReview
	}
	if l.unit {
		root.unit = walked.newUnit(path, l, entry)
	}
	if entry.directory() {
		walked.walk(root)
	} else {
		root.path, root.lower = parent, model.NormalizedKey(parent)
		w.measure(entry, &root, &bucket)
	}
	if bucket.Bytes > 0 {
		w.snapshot.Records = append(w.snapshot.Records, bucket)
	}
	for _, item := range walked.children {
		if item.Bytes >= model.FileFloor {
			w.snapshot.Records = append(w.snapshot.Records, *item)
		}
	}
	reviews := walked.reviews
	if firstReview != nil {
		reviews = append(reviews, firstReview)
	}
	for _, item := range reviews {
		if item.Bytes >= units.MiB {
			candidate := *item
			candidate.Series = model.Candidates
			w.snapshot.Records = append(w.snapshot.Records, candidate)
		}
	}
	for _, item := range walked.units {
		if item.Bytes >= units.MiB {
			w.snapshot.Records = append(w.snapshot.Records, *item)
		}
	}
}

// measure adds one file to the readings that contain it and to its category.
func (w *walker) measure(entry dirEntry, at *branch, bucket *model.Record) {
	coverage := &w.snapshot.Coverage
	coverage.Checked++
	size := entry.Bytes
	if size == 0 && entry.Length > 0 && entry.Attrs&attrsOffline != 0 {
		coverage.Online++
	}
	if size > 0 && entry.ID != (fileID{}) {
		if _, seen := w.sized[entry.ID]; seen {
			coverage.Shared++
			size = 0
		} else {
			w.sized[entry.ID] = struct{}{}
		}
	}
	addAllocation(bucket, size, entry.Modified)
	if at.review != nil && at.review != bucket {
		addAllocation(at.review, size, entry.Modified)
	}
	if at.child != nil {
		addAllocation(at.child, size, entry.Modified)
	}
	if at.unit != nil {
		addAllocation(at.unit, size, entry.Modified)
	}
	kind := at.kind
	// Files of a megabyte or more get their own label, such as a log or a
	// virtual disk inside an app folder; smaller files count with their folder.
	if bucket.Directory && size >= units.MiB {
		kind = classify(entry.Name, place{kind: at.kind, lower: at.lower}, false).kind
	}
	w.count(kind, size)
	if size < model.FileFloor {
		return
	}
	item := *bucket
	if bucket.Directory {
		item = pathRecord(filepath.Join(at.path, entry.Name), kind, entry)
		item.Bytes, item.Files = size, 1
	}
	item.Series = model.LargeFiles
	w.snapshot.Records = append(w.snapshot.Records, item)
	if at.review == nil && model.IsCandidate(item.Kind) {
		item.Series = model.Candidates
		w.snapshot.Records = append(w.snapshot.Records, item)
	}
}

// branch is a folder waiting to be listed inside a top-level reading.
type branch struct {
	path, lower, kind string
	entry             dirEntry
	review            *model.Record // The review candidate that contains this folder.
	child             *model.Record // The immediate subfolder of the reading that contains it.
	unit              *model.Record // The innermost labeled folder that contains it.
}

// tree walks the folders inside one top-level reading without recursion.
type tree struct {
	w        *walker
	bucket   *model.Record
	pending  []branch
	listing  []dirEntry
	reviews  []*model.Record
	children []*model.Record
	units    []*model.Record
}

func (t *tree) walk(root branch) {
	t.pending = []branch{root}
	for len(t.pending) != 0 && t.w.ctx.Err() == nil {
		current := t.pending[len(t.pending)-1]
		t.pending = t.pending[:len(t.pending)-1]
		t.w.mark()
		// The whole listing is read first, so markers such as a .git folder
		// label the folder before any of its files are counted.
		t.listing = t.listing[:0]
		err := t.w.reader.entries(t.w.ctx, current.path, units.MiB, func(child dirEntry) {
			t.listing = append(t.listing, child)
		})
		t.w.reader.addGap(&t.w.snapshot.Coverage, current.path, err)
		t.relabel(&current)
		for _, child := range t.listing {
			switch {
			case child.link():
				t.w.snapshot.Coverage.Links++
			case child.directory():
				t.descend(&current, child)
			default:
				t.w.measure(child, &current, t.bucket)
			}
		}
	}
}

// relabel applies what a folder's listing shows about it, such as a version
// control folder that marks a project, to the folder and its readings.
func (t *tree) relabel(current *branch) {
	l, ok := contentLabel(t.listing, current.kind)
	if !ok {
		return
	}
	current.kind = l.kind
	for _, item := range []*model.Record{t.bucket, current.child, current.review, current.unit} {
		if item != nil && item.Path == current.path {
			item.Kind, item.Detail = l.kind, l.why
		}
	}
	if current.review == nil && model.IsCandidate(l.kind) {
		item := pathRecord(current.path, l.kind, current.entry)
		item.Detail = l.why
		current.review = &item
		t.reviews = append(t.reviews, current.review)
	}
	if l.unit && (current.unit == nil || (current.unit.Kind != l.kind && current.unit.Path != current.path)) {
		current.unit = t.newUnit(current.path, l, current.entry)
	}
}

// descend queues a subfolder, starting a subfolder reading for immediate
// children, a review candidate for the first cleanup category on the way
// down, and a labeled folder where a program, game or project starts.
func (t *tree) descend(current *branch, child dirEntry) {
	path := filepath.Join(current.path, child.Name)
	l := classify(child.Name, place{kind: current.kind, lower: current.lower}, true)
	next := branch{path: path, lower: current.lower + `\` + strings.ToLower(child.Name), kind: l.kind, entry: child,
		review: current.review, child: current.child, unit: current.unit}
	if current.path == t.bucket.Path {
		item := pathRecord(path, l.kind, child)
		item.Series, item.Detail = model.Subfolders, l.why
		next.child = &item
		t.children = append(t.children, next.child)
	}
	if next.review == nil && model.IsCandidate(l.kind) {
		item := pathRecord(path, l.kind, child)
		item.Detail = l.why
		next.review = &item
		t.reviews = append(t.reviews, next.review)
	}
	if l.unit && (next.unit == nil || next.unit.Kind != l.kind) {
		next.unit = t.newUnit(path, l, child)
	}
	t.pending = append(t.pending, next)
}

// newUnit starts a labeled folder. Its files count toward it until a
// folder of another labeled category starts inside it.
func (t *tree) newUnit(path string, l label, entry dirEntry) *model.Record {
	item := pathRecord(path, l.kind, entry)
	item.Series, item.Name, item.Detail = model.Labeled, filepath.Base(path), l.why
	t.units = append(t.units, &item)
	return &item
}

func addAllocation(item *model.Record, size, modified int64) {
	item.Bytes += size
	item.Files++
	item.Modified = max(item.Modified, modified)
}

func pathRecord(path, kind string, entry dirEntry) model.Record {
	key := model.NormalizedKey(path)
	if entry.ID != (fileID{}) {
		key = fmt.Sprintf("%s%016x%016x", model.VolumeRoot(path), entry.ID.High, entry.ID.Low)
	}
	name := path
	if parent := filepath.Dir(path); parent != model.VolumeRoot(parent) {
		name = filepath.Base(parent) + " / " + filepath.Base(path)
	}
	return model.Record{Series: model.Folders, Key: key, Name: name, Path: path, Kind: kind,
		Directory: entry.directory(), Modified: entry.Modified}
}

// RefreshRecommended reports whether a saved capture is likely out of date:
// it is missing, older than a day, or the drives show activity since it was
// taken. Captures younger than ten minutes are always reused.
func RefreshRecommended(saved model.Snapshot, current []model.DriveState) bool {
	if saved.ID == 0 || !saved.Coverage.Subfolders {
		return true
	}
	age := time.Since(time.UnixMilli(saved.Captured))
	if age < 0 || age >= 24*time.Hour {
		return true
	}
	if age < 10*time.Minute {
		return false
	}
	if len(saved.Coverage.Drives) != len(current) {
		return true
	}
	for index, drive := range current {
		old := saved.Coverage.Drives[index]
		if drive.ID != old.ID || drive.Journal == 0 || drive.Journal != old.Journal ||
			drive.NextUSN != old.NextUSN || drive.Free != old.Free {
			return true
		}
	}
	return false
}
