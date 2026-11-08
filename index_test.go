package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndexWriterKeepsReadersConsistentAndRejectsStaleCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "history #100%.sqlite3")
	reader, err := openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.db.Close()
	writer, err := openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.db.Close()
	if stats := reader.db.Stats(); stats.MaxOpenConnections != 2 {
		t.Fatalf("expected WAL with a separate read connection, got %+v", stats)
	}
	first := snapshot{Scope: "scope", Label: "scope", Captured: 1000,
		Coverage: coverage{ScopeKind: "files"}, Records: []record{{Series: folders, Key: "folder", Bytes: mib}}}
	if err := writer.save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	tx, err := reader.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
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
	second.Records = []record{{Series: folders, Key: "folder", Bytes: 3 * mib}}
	if err := writer.save(ctx, &second); err != nil {
		t.Fatalf("writer could not commit while a reader held an earlier snapshot: %v", err)
	}
	checkPinned()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.ID = 0
	if err := writer.save(ctx, &stale); err == nil || stale.ID != 0 {
		t.Fatalf("stale capture was accepted: id=%d, err=%v", stale.ID, err)
	}
	diff, err := reader.compare(ctx, first.Scope, 0)
	if err != nil || diff.Baseline.ID != first.ID || diff.Latest.ID != second.ID ||
		len(diff.Changes) != 1 || diff.Changes[0].Bytes != 2*mib {
		t.Fatalf("concurrent history is inconsistent: %+v, %v", diff, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SQLite did not preserve the literal index filename: %v", err)
	}
}

func TestIndexSaveRollsBackAfterWritingSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	idx, err := openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.db.Close()
	first := snapshot{Scope: "scope", Label: "Scope", Captured: 1000,
		Coverage: coverage{ScopeKind: "files"}, Records: []record{{Series: folders, Key: "old", Bytes: mib}}}
	if err := idx.save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(`CREATE TEMP TRIGGER refuse_reading BEFORE INSERT ON readings
		WHEN NEW.item_key = 'new' BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	second := snapshot{Scope: first.Scope, Label: first.Label, Captured: 2000, Coverage: first.Coverage,
		Records: []record{{Series: folders, Key: "new", Bytes: 2 * mib}}}
	if err := idx.save(ctx, &second); err == nil || second.ID != 0 {
		t.Fatalf("failed capture was published: %d, %v", second.ID, err)
	}
	if err := idx.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var snapshots, readings int
	if err := reopened.db.QueryRow(`SELECT (SELECT count(*) FROM snapshots), (SELECT count(*) FROM readings)`).Scan(&snapshots, &readings); err != nil {
		t.Fatal(err)
	}
	latest, err := reopened.latest(ctx, first.Scope)
	if err != nil || snapshots != 1 || readings != 1 || latest.ID != first.ID || len(latest.Records) != 1 || latest.Records[0].Key != "old" {
		t.Fatalf("partial save survived restart: snapshots=%d readings=%d latest=%+v err=%v", snapshots, readings, latest, err)
	}
}

func TestIndexWindowsAndUnchangedReadings(t *testing.T) {
	idx, err := openIndex(context.Background(), filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.db.Close()
	ctx := context.Background()
	now := time.Now()
	sample := func(days int, bytes int64) snapshot {
		s := snapshot{Scope: "test", Label: "Test", Captured: now.AddDate(0, 0, -days).UnixMilli(),
			Coverage: coverage{ScopeKind: "files"},
			Records:  []record{{Series: folders, Key: "same", Name: "Test", Bytes: bytes}}}
		if err := idx.save(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	old := sample(8, 1<<30)
	second := sample(2, 2<<30)
	sample(0, 2<<30)

	latest, err := idx.latest(ctx, "test")
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
		diff, err := idx.compare(ctx, "test", want.days)
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
	before := map[rowKey]record{
		{folders, "same"}:     {Series: folders, Path: `C:\original`, Bytes: 4 << 30},
		{folders, "blocked"}:  {Series: folders, Path: `C:\blocked`, Bytes: 8 << 30},
		{largeFiles, "faded"}: {Series: largeFiles, Path: `D:\old.bin`, Bytes: 1 << 30},
	}
	after := map[rowKey]record{
		{folders, "same"}:   {Series: folders, Path: `C:\moved`, Bytes: 4 << 30},
		{largeFiles, "new"}: {Series: largeFiles, Path: `D:\new.bin`, Bytes: 2 << 30},
	}
	deltas, omitted, err := calculateChanges(context.Background(), before, after, coverage{}, coverage{Gaps: []string{`C:\blocked\restricted`}})
	if err != nil || omitted != 1 || len(deltas) != 2 {
		t.Fatalf("changes = %+v, omitted = %d, err = %v", deltas, omitted, err)
	}
	if !deltas[0].Bounded || deltas[0].Bytes != (2<<30)-fileFloor+1 ||
		!deltas[1].Bounded || deltas[1].Bytes != -((1<<30)-fileFloor+1) {
		t.Fatalf("expected conservative growth and shrinkage bounds: %+v", deltas)
	}
}

func TestVersionOneUpgradeAndEndpointChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	idx, err := openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.db.Close() }()
	now := time.Now()
	save := func(scope string, days int, subfoldersIndexed bool, rows ...record) snapshot {
		t.Helper()
		s := snapshot{Scope: scope, Label: scope, Captured: now.AddDate(0, 0, -days).UnixMilli(),
			Coverage: coverage{ScopeKind: "files", Subfolders: subfoldersIndexed}, Records: rows}
		if err := idx.save(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cache := record{Series: folders, Key: "old-identity", Name: "Cache", Path: `C:\root\Cache`, Bytes: 8 << 30}
	stable := record{Series: folders, Key: "stable", Name: "Stable", Bytes: 200 << 30}
	reverted := record{Series: folders, Key: "reverted", Name: "Reverted", Bytes: 5 << 30}
	old := save("source", 8, false, cache, stable, reverted)
	if _, err := idx.db.Exec("DROP INDEX readings_closed; PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := idx.db.Close(); err != nil {
		t.Fatal(err)
	}
	idx, err = openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := idx.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	save("unrelated", 7, true, record{Series: folders, Key: "foreign", Bytes: 99 << 30})
	middle := reverted
	middle.Bytes += 1 << 30
	child := record{Series: subfolders, Key: "child", Name: "Child", Path: `C:\root\Cache\Child`, Bytes: 3 << 30}
	save("source", 3, true, cache, stable, middle, child, record{Series: folders, Key: "temporary", Bytes: 4 << 30})
	replacement := cache
	replacement.Key, replacement.Path, replacement.Bytes = "new-identity", `c:\root\cache`, 10<<30
	child.Bytes += 1 << 30
	save("source", 0, true, replacement, stable, reverted, child)

	diff, err := idx.compare(ctx, "source", 7)
	if err != nil || diff.Baseline.ID != old.ID || len(diff.Changes) != 1 {
		t.Fatalf("unexpected endpoint changes: %+v, %v", diff, err)
	}
	got := diff.Changes[0]
	if got.Bytes != 2<<30 || got.Before.Key != cache.Key || got.After.Key != replacement.Key || got.Bounded {
		t.Fatalf("replacement was not paired: %+v", got)
	}
	diff, err = idx.compare(ctx, "source", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range diff.Changes {
		if got.Series == subfolders && got.Bytes == 1<<30 {
			return
		}
	}
	t.Fatalf("two capable captures should report subfolder growth: %+v", diff.Changes)
}

func TestIndexSavesAfterClockMovesBack(t *testing.T) {
	ctx := context.Background()
	idx, err := openIndex(ctx, filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.db.Close()
	save := func(captured time.Time, bytes int64) (snapshot, error) {
		s := snapshot{Scope: "scope", Label: "Scope", Captured: captured.UnixMilli(), Coverage: coverage{ScopeKind: "files"},
			Records: []record{{Series: folders, Key: "folder", Bytes: bytes}}}
		err := idx.save(ctx, &s)
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
	diff, err := idx.compare(ctx, "scope", 0)
	if err != nil || diff.Baseline.ID != behind.ID || diff.Latest.ID != fixed.ID ||
		len(diff.Changes) != 1 || diff.Changes[0].Bytes != mib {
		t.Fatalf("comparison after the clock was fixed: %+v, %v", diff, err)
	}
}

func TestQuickReviewComparesOnlyMatchingDrives(t *testing.T) {
	ctx := context.Background()
	c, usb := driveState{Root: `C:\`, ID: "c"}, driveState{Root: `E:\`, ID: "usb"}
	before := map[rowKey]record{
		{folders, "work"}: {Series: folders, Key: "work", Path: `C:\work`, Bytes: 1 << 30},
		{folders, "usb"}:  {Series: folders, Key: "usb", Path: `E:\backups`, Bytes: 50 << 30},
	}
	after := map[rowKey]record{
		{folders, "work"}: {Series: folders, Key: "work", Path: `C:\work`, Bytes: 2 << 30},
	}
	changes, omitted, err := calculateChanges(ctx, before, after,
		coverage{Drives: []driveState{c, usb}}, coverage{Drives: []driveState{c}})
	if err != nil || omitted != 1 || len(changes) != 1 || changes[0].Bytes != 1<<30 {
		t.Fatalf("an unplugged drive should not count as freed space: %+v, omitted %d, %v", changes, omitted, err)
	}
	after[rowKey{folders, "usb"}] = record{Series: folders, Key: "usb", Path: `E:\backups`, Bytes: 10 << 30}
	changes, omitted, err = calculateChanges(ctx, before, after,
		coverage{Drives: []driveState{c, usb}}, coverage{Drives: []driveState{c, {Root: `E:\`, ID: "other"}}})
	if err != nil || omitted != 1 || len(changes) != 1 || changes[0].Bytes != 1<<30 {
		t.Fatalf("a different volume at the same letter is not comparable: %+v, omitted %d, %v", changes, omitted, err)
	}
}

func TestQuickReviewHistoryMovesToDriveIndependentKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	idx, err := openIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.db.Close() }()
	now := time.Now()
	save := func(scope string, days int, bytes int64) snapshot {
		t.Helper()
		s := snapshot{Scope: scope, Label: quickScope, Captured: now.AddDate(0, 0, -days).UnixMilli(),
			Coverage: coverage{ScopeKind: "files"}, Records: []record{{Series: folders, Key: "work", Bytes: bytes}}}
		if err := idx.save(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	older := `files-v1|quick review|C:\=old`
	save(older, 9, 1<<30)
	current := save(`files-v1|quick review|C:\=c|E:\=usb`, 2, 2<<30)
	if err := idx.db.Close(); err != nil {
		t.Fatal(err)
	}
	if idx, err = openIndex(ctx, path); err != nil {
		t.Fatal(err)
	}
	key := scopeKey(quickScope, nil)
	latest, err := idx.latest(ctx, key)
	if err != nil || latest.ID != current.ID || len(latest.Records) != 1 || latest.Records[0].Bytes != 2<<30 {
		t.Fatalf("the most recent Quick review history should move to %q: %+v, %v", key, latest, err)
	}
	next := save(key, 0, 3<<30)
	diff, err := idx.compare(ctx, key, 0)
	if err != nil || diff.Baseline.ID != current.ID || diff.Latest.ID != next.ID ||
		len(diff.Changes) != 1 || diff.Changes[0].Bytes != 1<<30 {
		t.Fatalf("moved history should compare with new captures: %+v, %v", diff, err)
	}
	if saved, err := idx.latest(ctx, older); err != nil || saved.Scope != older {
		t.Fatalf("older drive sets keep their own history: %+v, %v", saved, err)
	}
}

func TestFolderFilterUsesPathBoundaries(t *testing.T) {
	filter := parseFilter(`path:C:\Project A`)
	for _, path := range []string{`c:\project a`, `C:\Project A\Cache`} {
		if !filter.matches(record{Path: path}, "") {
			t.Fatalf("expected match for %s", path)
		}
	}
	for _, path := range []string{"", `C:\Project AB\Cache`, `D:\Project A\Cache`} {
		if filter.matches(record{Path: path}, "") {
			t.Fatalf("unexpected match for %s", path)
		}
	}
}
