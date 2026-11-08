package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const mib = units.MiB

func openTest(t *testing.T, path string) *Index {
	t.Helper()
	idx, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestWriterKeepsReadersConsistentAndRejectsStaleCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "history #100%.sqlite3")
	reader, writer := openTest(t, path), openTest(t, path)
	if stats := reader.db.Stats(); stats.MaxOpenConnections != 2 {
		t.Fatalf("expected WAL with a separate read connection, got %+v", stats)
	}
	first := model.Snapshot{Scope: "scope", Label: "scope", Captured: 1000,
		Coverage: model.Coverage{ScopeKind: model.ScopeFiles}, Records: []model.Record{{Series: model.Folders, Key: "folder", Bytes: mib}}}
	if err := writer.Save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	tx, err := reader.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	checkPinned := func() {
		t.Helper()
		var id int64
		if err := tx.QueryRowContext(ctx, "SELECT MAX(id) FROM snapshots").Scan(&id); err != nil || id != first.ID {
			t.Fatalf("reader should keep capture %d, got %d (%v)", first.ID, id, err)
		}
	}
	checkPinned()
	second := first
	second.ID, second.Captured = 0, first.Captured+1
	second.Records = []model.Record{{Series: model.Folders, Key: "folder", Bytes: 3 * mib}}
	if err := writer.Save(ctx, &second); err != nil {
		t.Fatalf("writer could not commit while a reader held an earlier snapshot: %v", err)
	}
	checkPinned()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.ID = 0
	if err := writer.Save(ctx, &stale); !errors.Is(err, model.ErrConflict) || stale.ID != 0 {
		t.Fatalf("stale capture was accepted: id=%d, err=%v", stale.ID, err)
	}
	diff, err := reader.Compare(ctx, first.Scope, 0)
	if err != nil || diff.Baseline.ID != first.ID || diff.Latest.ID != second.ID ||
		len(diff.Changes) != 1 || diff.Changes[0].Bytes != 2*mib {
		t.Fatalf("concurrent history is inconsistent: %+v, %v", diff, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SQLite did not preserve the literal index filename: %v", err)
	}
}

func TestSaveRollsBackAfterWritingSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	idx, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	first := model.Snapshot{Scope: "scope", Label: "Scope", Captured: 1000,
		Coverage: model.Coverage{ScopeKind: model.ScopeFiles}, Records: []model.Record{{Series: model.Folders, Key: "old", Bytes: mib}}}
	if err := idx.Save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(`CREATE TEMP TRIGGER refuse_reading BEFORE INSERT ON readings
		WHEN NEW.item_key = 'new' BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	second := model.Snapshot{Scope: first.Scope, Label: first.Label, Captured: 2000, Coverage: first.Coverage,
		Records: []model.Record{{Series: model.Folders, Key: "new", Bytes: 2 * mib}}}
	if err := idx.Save(ctx, &second); err == nil || second.ID != 0 {
		t.Fatalf("failed capture was published: %d, %v", second.ID, err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTest(t, path)
	var snapshots, readings int
	if err := reopened.db.QueryRow(`SELECT (SELECT count(*) FROM snapshots), (SELECT count(*) FROM readings)`).Scan(&snapshots, &readings); err != nil {
		t.Fatal(err)
	}
	latest, err := reopened.Latest(ctx, first.Scope)
	if err != nil || snapshots != 1 || readings != 1 || latest.ID != first.ID || len(latest.Records) != 1 || latest.Records[0].Key != "old" {
		t.Fatalf("partial save survived restart: snapshots=%d readings=%d latest=%+v err=%v", snapshots, readings, latest, err)
	}
}

func TestMissingScopeIsNotFound(t *testing.T) {
	idx := openTest(t, filepath.Join(t.TempDir(), "history.sqlite3"))
	if _, err := idx.Latest(context.Background(), "missing"); !errors.Is(err, model.ErrNotFound) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("latest of a missing scope: %v", err)
	}
	if _, err := idx.Compare(context.Background(), "missing", 0); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("comparison of a missing scope: %v", err)
	}
}

func TestWindowsAndUnchangedReadings(t *testing.T) {
	idx := openTest(t, filepath.Join(t.TempDir(), "history.sqlite3"))
	ctx := context.Background()
	now := time.Now()
	sample := func(days int, bytes int64) model.Snapshot {
		s := model.Snapshot{Scope: "test", Label: "Test", Captured: now.AddDate(0, 0, -days).UnixMilli(),
			Coverage: model.Coverage{ScopeKind: model.ScopeFiles},
			Records:  []model.Record{{Series: model.Folders, Key: "same", Name: "Test", Bytes: bytes}}}
		if err := idx.Save(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	old := sample(8, 1<<30)
	second := sample(2, 2<<30)
	sample(0, 2<<30)

	latest, err := idx.Latest(ctx, "test")
	if err != nil || len(latest.Records) != 1 || latest.Records[0].Bytes != 2<<30 {
		t.Fatalf("latest capture: %+v, %v", latest, err)
	}
	var revisions int
	if err := idx.db.QueryRow("SELECT count(*) FROM readings").Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("expected two changed versions, got %d: %v", revisions, err)
	}
	for _, want := range []struct {
		days  int
		id    int64
		delta int64
		short bool
	}{{0, second.ID, 0, false}, {1, second.ID, 0, false}, {7, old.ID, 1 << 30, false}, {30, old.ID, 1 << 30, true}} {
		diff, err := idx.Compare(ctx, "test", want.days)
		if err != nil || diff.Baseline.ID != want.id || diff.ShortWindow != want.short {
			t.Fatalf("comparison %d: %+v, %v", want.days, diff, err)
		}
		if want.delta == 0 && len(diff.Changes) != 0 ||
			want.delta != 0 && (len(diff.Changes) != 1 || diff.Changes[0].Bytes != want.delta) {
			t.Fatalf("comparison %d changes: %+v", want.days, diff.Changes)
		}
	}
}

func TestDeltaTracksIdentityAndCoverage(t *testing.T) {
	before := map[model.RowKey]model.Record{
		{Series: model.Folders, Key: "same"}:     {Series: model.Folders, Path: `C:\original`, Bytes: 4 << 30},
		{Series: model.Folders, Key: "blocked"}:  {Series: model.Folders, Path: `C:\blocked`, Bytes: 8 << 30},
		{Series: model.LargeFiles, Key: "faded"}: {Series: model.LargeFiles, Path: `D:\old.bin`, Bytes: 1 << 30},
	}
	after := map[model.RowKey]model.Record{
		{Series: model.Folders, Key: "same"}:   {Series: model.Folders, Path: `C:\moved`, Bytes: 4 << 30},
		{Series: model.LargeFiles, Key: "new"}: {Series: model.LargeFiles, Path: `D:\new.bin`, Bytes: 2 << 30},
	}
	deltas, omitted, err := calculateChanges(context.Background(), before, after,
		model.Coverage{}, model.Coverage{Gaps: []string{`C:\blocked\restricted`}})
	if err != nil || omitted != 1 || len(deltas) != 2 {
		t.Fatalf("changes = %+v, omitted = %d, err = %v", deltas, omitted, err)
	}
	if !deltas[0].Bounded || deltas[0].Bytes != (2<<30)-model.FileFloor+1 ||
		!deltas[1].Bounded || deltas[1].Bytes != -((1<<30)-model.FileFloor+1) {
		t.Fatalf("expected conservative growth and shrinkage bounds: %+v", deltas)
	}
}

func TestPartialSizesAreOmitted(t *testing.T) {
	key := model.RowKey{Series: model.Projects, Key: "alpha"}
	old := model.Record{Series: model.Projects, Key: "alpha", Bytes: 200}
	partial := model.Record{Series: model.Projects, Key: "alpha", Bytes: 10, Partial: true}
	changes, omitted, err := calculateChanges(context.Background(), map[model.RowKey]model.Record{key: old},
		map[model.RowKey]model.Record{key: partial}, model.Coverage{}, model.Coverage{})
	if err != nil || len(changes) != 0 || omitted != 1 {
		t.Fatalf("partial sizes should not produce a false drop: %+v, omitted %d, err = %v", changes, omitted, err)
	}
}

func TestVersionOneUpgradeAndEndpointChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	idx, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	now := time.Now()
	save := func(scope string, days int, subfoldersIndexed bool, rows ...model.Record) model.Snapshot {
		t.Helper()
		s := model.Snapshot{Scope: scope, Label: scope, Captured: now.AddDate(0, 0, -days).UnixMilli(),
			Coverage: model.Coverage{ScopeKind: model.ScopeFiles, Subfolders: subfoldersIndexed}, Records: rows}
		if err := idx.Save(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cache := model.Record{Series: model.Folders, Key: "old-identity", Name: "Cache", Path: `C:\root\Cache`, Bytes: 8 << 30}
	stable := model.Record{Series: model.Folders, Key: "stable", Name: "Stable", Bytes: 200 << 30}
	reverted := model.Record{Series: model.Folders, Key: "reverted", Name: "Reverted", Bytes: 5 << 30}
	old := save("source", 8, false, cache, stable, reverted)
	if _, err := idx.db.Exec("DROP INDEX readings_closed; PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if idx, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := idx.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	save("unrelated", 7, true, model.Record{Series: model.Folders, Key: "foreign", Bytes: 99 << 30})
	middle := reverted
	middle.Bytes += 1 << 30
	child := model.Record{Series: model.Subfolders, Key: "child", Name: "Child", Path: `C:\root\Cache\Child`, Bytes: 3 << 30}
	save("source", 3, true, cache, stable, middle, child, model.Record{Series: model.Folders, Key: "temporary", Bytes: 4 << 30})
	replacement := cache
	replacement.Key, replacement.Path, replacement.Bytes = "new-identity", `c:\root\cache`, 10<<30
	child.Bytes += 1 << 30
	save("source", 0, true, replacement, stable, reverted, child)

	diff, err := idx.Compare(ctx, "source", 7)
	if err != nil || diff.Baseline.ID != old.ID || len(diff.Changes) != 1 {
		t.Fatalf("unexpected endpoint changes: %+v, %v", diff, err)
	}
	got := diff.Changes[0]
	if got.Bytes != 2<<30 || got.Before.Key != cache.Key || got.After.Key != replacement.Key || got.Bounded {
		t.Fatalf("replacement was not paired: %+v", got)
	}
	diff, err = idx.Compare(ctx, "source", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range diff.Changes {
		if got.Series == model.Subfolders && got.Bytes == 1<<30 {
			return
		}
	}
	t.Fatalf("two capable captures should report subfolder growth: %+v", diff.Changes)
}

func TestSavesAfterClockMovesBack(t *testing.T) {
	ctx := context.Background()
	idx := openTest(t, filepath.Join(t.TempDir(), "history.sqlite3"))
	save := func(captured time.Time, bytes int64) (model.Snapshot, error) {
		s := model.Snapshot{Scope: "scope", Label: "Scope", Captured: captured.UnixMilli(), Coverage: model.Coverage{ScopeKind: model.ScopeFiles},
			Records: []model.Record{{Series: model.Folders, Key: "folder", Bytes: bytes}}}
		err := idx.Save(ctx, &s)
		return s, err
	}
	// These captures were stamped while the clock ran 200 days ahead.
	ahead := time.Now().AddDate(0, 0, 200)
	first, err := save(ahead, mib)
	if err != nil {
		t.Fatal(err)
	}
	second, err := save(ahead.Add(time.Hour), 2*mib)
	if err != nil {
		t.Fatal(err)
	}
	behind, err := save(time.Now(), 3*mib)
	if err != nil {
		t.Fatalf("a capture after the clock moved back was rejected: %v", err)
	}
	fixed, err := save(ahead.Add(2*time.Hour), 4*mib)
	if err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := idx.db.QueryRow("SELECT count(*) FROM snapshots WHERE id IN (?, ?, ?, ?)",
		first.ID, second.ID, behind.ID, fixed.ID).Scan(&kept); err != nil || kept != 4 {
		t.Fatalf("a capture with an older timestamp expired the history before it: %d of 4 kept, %v", kept, err)
	}
	diff, err := idx.Compare(ctx, "scope", 0)
	if err != nil || diff.Baseline.ID != behind.ID || diff.Latest.ID != fixed.ID ||
		len(diff.Changes) != 1 || diff.Changes[0].Bytes != mib {
		t.Fatalf("comparison after the clock was fixed: %+v, %v", diff, err)
	}
}

func TestQuickReviewComparesOnlyMatchingDrives(t *testing.T) {
	ctx := context.Background()
	c, usb := model.DriveState{Root: `C:\`, ID: "c"}, model.DriveState{Root: `E:\`, ID: "usb"}
	work := model.RowKey{Series: model.Folders, Key: "work"}
	backups := model.RowKey{Series: model.Folders, Key: "usb"}
	before := map[model.RowKey]model.Record{
		work:    {Series: model.Folders, Key: "work", Path: `C:\work`, Bytes: 1 << 30},
		backups: {Series: model.Folders, Key: "usb", Path: `E:\backups`, Bytes: 50 << 30},
	}
	after := map[model.RowKey]model.Record{
		work: {Series: model.Folders, Key: "work", Path: `C:\work`, Bytes: 2 << 30},
	}
	changes, omitted, err := calculateChanges(ctx, before, after,
		model.Coverage{Drives: []model.DriveState{c, usb}}, model.Coverage{Drives: []model.DriveState{c}})
	if err != nil || omitted != 1 || len(changes) != 1 || changes[0].Bytes != 1<<30 {
		t.Fatalf("an unplugged drive should not count as freed space: %+v, omitted %d, %v", changes, omitted, err)
	}
	after[backups] = model.Record{Series: model.Folders, Key: "usb", Path: `E:\backups`, Bytes: 10 << 30}
	changes, omitted, err = calculateChanges(ctx, before, after,
		model.Coverage{Drives: []model.DriveState{c, usb}}, model.Coverage{Drives: []model.DriveState{c, {Root: `E:\`, ID: "other"}}})
	if err != nil || omitted != 1 || len(changes) != 1 || changes[0].Bytes != 1<<30 {
		t.Fatalf("a different volume at the same letter is not comparable: %+v, omitted %d, %v", changes, omitted, err)
	}
}

func TestQuickReviewHistoryMovesToDriveIndependentKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	idx, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	now := time.Now()
	save := func(scope string, days int, bytes int64) model.Snapshot {
		t.Helper()
		s := model.Snapshot{Scope: scope, Label: model.QuickScope, Captured: now.AddDate(0, 0, -days).UnixMilli(),
			Coverage: model.Coverage{ScopeKind: model.ScopeFiles}, Records: []model.Record{{Series: model.Folders, Key: "work", Bytes: bytes}}}
		if err := idx.Save(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	older := `files-v1|quick review|C:\=old`
	save(older, 9, 1<<30)
	current := save(`files-v1|quick review|C:\=c|E:\=usb`, 2, 2<<30)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if idx, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	key := model.ScopeKey(model.QuickScope, nil)
	latest, err := idx.Latest(ctx, key)
	if err != nil || latest.ID != current.ID || len(latest.Records) != 1 || latest.Records[0].Bytes != 2<<30 {
		t.Fatalf("the most recent Quick review history should move to %q: %+v, %v", key, latest, err)
	}
	next := save(key, 0, 3<<30)
	diff, err := idx.Compare(ctx, key, 0)
	if err != nil || diff.Baseline.ID != current.ID || diff.Latest.ID != next.ID ||
		len(diff.Changes) != 1 || diff.Changes[0].Bytes != 1<<30 {
		t.Fatalf("moved history should compare with new captures: %+v, %v", diff, err)
	}
	if saved, err := idx.Latest(ctx, older); err != nil || saved.Scope != older {
		t.Fatalf("older drive sets keep their own history: %+v, %v", saved, err)
	}
}
