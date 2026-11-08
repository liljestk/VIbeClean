package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	quickScope = "Quick review"
	mib        = int64(1 << 20)
	fileFloor  = 64 * mib
	minChange  = 256 * mib
	folders    = "Folders"
	subfolders = "Subfolders"
	candidates = "Review candidates"
	largeFiles = "Large files"
	dockerSum  = "Docker totals"
	projects   = "Projects"
	volumes    = "Volumes"
	containers = "Containers"
	images     = "Images"
	buildCache = "Build cache"
)

type record struct {
	Series      string `json:"series"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Bytes       int64  `json:"bytes"`
	Files       int64  `json:"files,omitempty"`
	Modified    int64  `json:"modified_ms,omitempty"`
	Directory   bool   `json:"directory,omitempty"`
	Activity    string `json:"activity,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Group       string `json:"group,omitempty"`
	Idle        bool   `json:"idle,omitempty"`
	Reclaimable int64  `json:"reclaimable_bytes,omitempty"`
	Partial     bool   `json:"partial,omitempty"`
}

type driveState struct {
	Root    string `json:"root"`
	ID      string `json:"volume_id"`
	Total   int64  `json:"total_bytes"`
	Free    int64  `json:"free_bytes"`
	Journal uint64 `json:"journal_id,omitempty"`
	NextUSN int64  `json:"next_usn,omitempty"`
}

type coverage struct {
	Drives     []driveState `json:"drives,omitempty"`
	Gaps       []string     `json:"gaps,omitempty"`
	Checked    int64        `json:"files_checked,omitempty"`
	Shared     int64        `json:"repeated_links,omitempty"`
	Online     int64        `json:"online_only,omitempty"`
	Links      int64        `json:"skipped_links,omitempty"`
	Estimated  int64        `json:"estimated_dirs,omitempty"`
	Subfolders bool         `json:"subfolders,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Engine     string       `json:"engine,omitempty"`
	ScopeKind  string       `json:"scope_kind"`
}

type snapshot struct {
	ID       int64    `json:"id"`
	Scope    string   `json:"scope_id"`
	Label    string   `json:"label"`
	Captured int64    `json:"captured_ms"`
	Coverage coverage `json:"coverage"`
	Records  []record `json:"records,omitempty"`
}

type change struct {
	Series  string  `json:"series"`
	Before  *record `json:"before,omitempty"`
	After   *record `json:"after,omitempty"`
	Bytes   int64   `json:"delta_bytes"`
	Bounded bool    `json:"minimum_magnitude,omitempty"`
}

func (c change) item() record {
	if c.After != nil {
		return *c.After
	}
	return *c.Before
}

type comparison struct {
	Latest      snapshot `json:"latest"`
	Baseline    snapshot `json:"baseline"`
	ShortWindow bool     `json:"short_window"`
	Omitted     int      `json:"omitted_coverage_changes"`
	Changes     []change `json:"changes"`
}

type index struct {
	db *sql.DB
}

func defaultIndexPath() (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		var err error
		local, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(local, "SpaceReview", "index.sqlite3"), nil
}

func openIndex(ctx context.Context, path string) (_ *index, err error) {
	started := time.Now()
	stage, version := "prepare_path", 0
	var journal string
	defer func() {
		logDuration(ctx, "index.open", started, err, slog.String("stage", stage),
			slog.Int("prior_schema", version), slog.String("journal", journal))
	}()
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, "\\\\") {
		return nil, errors.New("choose a local path for the SQLite index")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	stage = "connect"
	uri := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path), RawQuery: url.Values{
		"_pragma": {"busy_timeout=5000", "synchronous=FULL"},
	}.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	stage = "check_schema"
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version > 2 {
		db.Close()
		if err == nil {
			err = errors.New("the index was created by a newer version of Space Review")
		}
		return nil, err
	}
	const schema = `
			CREATE TABLE IF NOT EXISTS snapshots (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				scope TEXT NOT NULL,
				label TEXT NOT NULL,
				captured INTEGER NOT NULL,
				coverage TEXT NOT NULL
			);
			CREATE INDEX IF NOT EXISTS snapshots_time ON snapshots(scope, captured, id);
			CREATE TABLE IF NOT EXISTS readings (
				scope TEXT NOT NULL,
				series TEXT NOT NULL,
				item_key TEXT NOT NULL,
				first INTEGER NOT NULL,
				until INTEGER,
				value TEXT NOT NULL,
				PRIMARY KEY (scope, series, item_key, first)
			) WITHOUT ROWID;
			CREATE UNIQUE INDEX IF NOT EXISTS readings_live
				ON readings(scope, series, item_key) WHERE until IS NULL;
			CREATE INDEX IF NOT EXISTS readings_time ON readings(scope, first, until);
	`
	const upgrade = `
		CREATE INDEX IF NOT EXISTS readings_closed ON readings(scope, until, first) WHERE until IS NOT NULL;
		PRAGMA user_version = 2;
	`
	if version < 2 {
		stage = "migrate"
		create := ""
		if version == 0 {
			create = schema
		}
		_, err = db.ExecContext(ctx, "BEGIN IMMEDIATE;"+create+upgrade+"COMMIT;")
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	stage = "journal_mode"
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		db.Close()
		return nil, err
	}
	if journal != "wal" && isFixed(volumeRoot(path)) {
		var newMode string
		if walErr := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&newMode); walErr == nil {
			journal = newMode
		} else {
			logDiagnostic(ctx, slog.LevelWarn, "index.wal_unavailable", slog.String("error_kind", diagnosticErrorKind(walErr)))
		}
	}
	if journal == "wal" {
		db.SetMaxOpenConns(2)
	}
	idx := &index{db: db}
	if adoptErr := idx.adoptQuickHistory(ctx); adoptErr != nil {
		logDiagnostic(ctx, slog.LevelWarn, "index.adopt_quick_failed", slog.String("error_kind", diagnosticErrorKind(adoptErr)))
	}
	return idx, nil
}

// Quick review history used to be keyed by the set of fixed drives. The most
// recent of those histories moves to the drive-independent key, once.
const legacyQuickScopes = "files-v1|quick review|%"

func (i *index) adoptQuickHistory(ctx context.Context) (err error) {
	key := scopeKey(quickScope, nil)
	var pending bool
	if err := i.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM snapshots WHERE scope = ?)
		AND EXISTS (SELECT 1 FROM snapshots WHERE scope LIKE ?)`, key, legacyQuickScopes).Scan(&pending); err != nil || !pending {
		return err
	}
	started := time.Now()
	defer func() { logDuration(ctx, "index.adopt_quick", started, err) }()
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Readings move first: the first statement takes the writer lock, and the
	// snapshot rows still name the legacy scope until the second statement.
	const legacy = `(SELECT scope FROM snapshots WHERE scope LIKE ? ORDER BY id DESC LIMIT 1)`
	const unclaimed = `NOT EXISTS (SELECT 1 FROM snapshots WHERE scope = ?)`
	for _, table := range []string{"readings", "snapshots"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET scope = ? WHERE scope = `+legacy+` AND `+unclaimed,
			key, legacyQuickScopes, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type rowKey struct {
	Series string
	Key    string
}

func (i *index) save(ctx context.Context, s *snapshot) (err error) {
	started := time.Now()
	stage := "encode"
	var encoded, writerWait, readActive, writeVersions, commit time.Duration
	var reused, closed, added int
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		logDuration(ctx, "index.save", started, err, slog.String("stage", stage),
			slog.Int64("capture_id", s.ID), slog.Int("readings", len(s.Records)),
			slog.Int("versions_reused", reused), slog.Int("versions_closed", closed), slog.Int("versions_added", added),
			slog.Float64("encode_ms", milliseconds(encoded)), slog.Float64("writer_wait_ms", milliseconds(writerWait)),
			slog.Float64("read_active_ms", milliseconds(readActive)), slog.Float64("write_versions_ms", milliseconds(writeVersions)),
			slog.Float64("commit_ms", milliseconds(commit)))
	}()
	if s.Captured == 0 {
		s.Captured = time.Now().UnixMilli()
	}
	meta, err := json.Marshal(s.Coverage)
	if err != nil {
		return err
	}
	next := make(map[rowKey]string, len(s.Records))
	for _, item := range s.Records {
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		next[rowKey{item.Series, item.Key}] = string(raw)
	}
	encoded = time.Since(started)
	waitStarted := time.Now()
	stage = "begin"
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Take the writer lock before reading the prior version. A capture that
	// finished after this one makes it stale. A saved time still ahead of the
	// clock means the clock moved back, which must not block every later save.
	stage = "insert_snapshot"
	result, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots(scope, label, captured, coverage)
		SELECT ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM snapshots WHERE scope = ? AND captured > ? AND captured <= ?)
	`, s.Scope, s.Label, s.Captured, string(meta), s.Scope, s.Captured, time.Now().UnixMilli())
	writerWait = time.Since(waitStarted)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return errors.New("a newer capture finished first")
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	stage = "read_active"
	phaseStarted := time.Now()
	active, err := readValues(ctx, tx, `SELECT series, item_key, value FROM readings
		WHERE scope = ? AND until IS NULL`, s.Scope)
	readActive = time.Since(phaseStarted)
	if err != nil {
		return err
	}
	stage = "write_versions"
	phaseStarted = time.Now()
	closeRow, err := tx.PrepareContext(ctx, `UPDATE readings SET until = ?
		WHERE scope = ? AND series = ? AND item_key = ? AND until IS NULL`)
	if err != nil {
		return err
	}
	defer closeRow.Close()
	for key, previous := range active {
		if value, ok := next[key]; ok && value == previous {
			delete(next, key)
			reused++
			continue
		}
		if _, err := closeRow.ExecContext(ctx, id, s.Scope, key.Series, key.Key); err != nil {
			return err
		}
		closed++
	}
	addRow, err := tx.PrepareContext(ctx, `
		INSERT INTO readings(scope, series, item_key, first, value) VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer addRow.Close()
	for key, value := range next {
		if _, err := addRow.ExecContext(ctx, s.Scope, key.Series, key.Key, id, value); err != nil {
			return err
		}
		added++
	}
	writeVersions = time.Since(phaseStarted)
	// Keep one baseline before the retention cutoff. Only captures older than
	// every capture inside the window expire, so one capture stamped by a wrong
	// clock cannot remove the history saved before it.
	stage = "retention"
	var anchor int64
	cutoff := s.Captured - (120 * 24 * time.Hour).Milliseconds()
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM snapshots WHERE scope = ? AND captured < ?
		AND id < (SELECT MIN(id) FROM snapshots WHERE scope = ? AND captured >= ?)`,
		s.Scope, cutoff, s.Scope, cutoff).Scan(&anchor)
	if err != nil {
		return err
	}
	if anchor > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE scope = ? AND id < ?`, s.Scope, anchor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM readings WHERE scope = ? AND until <= ?`, s.Scope, anchor); err != nil {
			return err
		}
	}
	stage = "commit"
	phaseStarted = time.Now()
	err = tx.Commit()
	commit = time.Since(phaseStarted)
	if err != nil {
		return err
	}
	s.ID = id
	return nil
}

func readValues(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[rowKey]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[rowKey]string)
	for rows.Next() {
		var key rowKey
		var value string
		if err := rows.Scan(&key.Series, &key.Key, &value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, rows.Err()
}

func loadSnapshot(row interface{ Scan(...any) error }) (snapshot, error) {
	var s snapshot
	var raw string
	if err := row.Scan(&s.ID, &s.Scope, &s.Label, &s.Captured, &raw); err != nil {
		return s, err
	}
	return s, json.Unmarshal([]byte(raw), &s.Coverage)
}

const snapshotFields = "id, scope, label, captured, coverage"

func (i *index) list(ctx context.Context) (entries []snapshot, err error) {
	started := time.Now()
	defer func() { logDuration(ctx, "index.list", started, err, slog.Int("scopes", len(entries))) }()
	rows, err := i.db.QueryContext(ctx, `SELECT `+snapshotFields+` FROM snapshots
		WHERE id IN (SELECT MAX(id) FROM snapshots GROUP BY scope)
		ORDER BY captured DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s, err := loadSnapshot(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, s)
	}
	return entries, rows.Err()
}

func (i *index) latest(ctx context.Context, scope string) (s snapshot, err error) {
	started := time.Now()
	defer func() {
		logDuration(ctx, "index.latest", started, err, slog.Int64("capture_id", s.ID), slog.Int("readings", len(s.Records)))
	}()
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot{}, err
	}
	defer tx.Rollback()
	s, err = loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots
		WHERE scope = ? ORDER BY id DESC LIMIT 1`, scope))
	if err != nil {
		return snapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT value FROM readings
		WHERE scope = ? AND until IS NULL`, s.Scope)
	if err != nil {
		return snapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return snapshot{}, err
		}
		var item record
		if err := json.Unmarshal(value, &item); err != nil {
			return snapshot{}, err
		}
		s.Records = append(s.Records, item)
	}
	if err := rows.Err(); err != nil {
		return snapshot{}, err
	}
	return s, tx.Commit()
}

func readRecords(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[rowKey]record, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[rowKey]record)
	for rows.Next() {
		var key rowKey
		var value []byte
		if err := rows.Scan(&key.Series, &key.Key, &value); err != nil {
			return nil, err
		}
		var item record
		if err := json.Unmarshal(value, &item); err != nil {
			return nil, err
		}
		items[key] = item
	}
	return items, rows.Err()
}

func (i *index) compare(ctx context.Context, scope string, days int) (result comparison, err error) {
	started := time.Now()
	stage := "begin"
	var earlierRows, laterRows int
	var readTime, matchTime time.Duration
	defer func() {
		logDuration(ctx, "index.compare", started, err, slog.String("stage", stage), slog.Int("days", days),
			slog.Int64("baseline_id", result.Baseline.ID), slog.Int64("capture_id", result.Latest.ID),
			slog.Int("baseline_versions", earlierRows), slog.Int("latest_versions", laterRows),
			slog.Int("changes", len(result.Changes)), slog.Int("omitted", result.Omitted), slog.Bool("short_window", result.ShortWindow),
			slog.Float64("read_ms", milliseconds(readTime)), slog.Float64("match_ms", milliseconds(matchTime)))
	}()
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	stage = "read_latest"
	result.Latest, err = loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots
		WHERE scope = ? ORDER BY id DESC LIMIT 1`, scope))
	if err != nil {
		return result, err
	}
	stage = "read_baseline"
	if days == 0 {
		result.Baseline, err = loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots
			WHERE scope = ? AND id < ? ORDER BY id DESC LIMIT 1`, scope, result.Latest.ID))
	} else {
		cutoff := result.Latest.Captured - (time.Duration(days) * 24 * time.Hour).Milliseconds()
		result.Baseline, err = loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots
			WHERE scope = ? AND captured <= ? ORDER BY id DESC LIMIT 1`, scope, cutoff))
		if errors.Is(err, sql.ErrNoRows) {
			result.ShortWindow = true
			result.Baseline, err = loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots
				WHERE scope = ? AND id < ? ORDER BY id LIMIT 1`, scope, result.Latest.ID))
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return result, tx.Commit()
	}
	if err != nil {
		return result, err
	}
	// An unchanged version spans both endpoints. Read only versions that end
	// or begin between them, ignoring intermediate versions already replaced.
	first, last := result.Baseline.ID, result.Latest.ID
	stage = "read_versions"
	older, err := readRecords(ctx, tx, `SELECT series, item_key, value FROM readings
		WHERE scope = ? AND until > ? AND until <= ? AND first <= ?`, scope, first, last, first)
	if err != nil {
		return result, err
	}
	earlierRows = len(older)
	newer, err := readRecords(ctx, tx, `SELECT series, item_key, value FROM readings
		WHERE scope = ? AND first > ? AND first <= ? AND (until IS NULL OR until > ?)`, scope, first, last, last)
	if err != nil {
		return result, err
	}
	laterRows = len(newer)
	stage = "end_read"
	if err := tx.Commit(); err != nil {
		return result, err
	}
	readTime = time.Since(started)
	stage = "match"
	matchStarted := time.Now()
	result.Changes, result.Omitted, err = calculateChanges(ctx, older, newer, result.Baseline.Coverage, result.Latest.Coverage)
	matchTime = time.Since(matchStarted)
	return result, err
}

func calculateChanges(ctx context.Context, older, newer map[rowKey]record, before, after coverage) ([]change, int, error) {
	gapsBefore, gapsAfter := make(map[string]bool), make(map[string]bool)
	for _, path := range before.Gaps {
		gapsBefore[normalizedKey(path)] = true
	}
	for _, path := range after.Gaps {
		gapsAfter[normalizedKey(path)] = true
	}
	changedGaps := make(map[string]bool)
	for gap := range gapsBefore {
		if !gapsAfter[gap] {
			changedGaps[gap] = true
		}
	}
	for gap := range gapsAfter {
		if !gapsBefore[gap] {
			changedGaps[gap] = true
		}
	}
	// Quick review spans every fixed drive. A path counts only when its drive
	// holds the same volume in both captures, so an added, removed or replaced
	// drive is not reported as growth or freed space.
	var sameVolume map[string]bool
	if len(before.Drives) > 0 && len(after.Drives) > 0 {
		sameVolume = make(map[string]bool)
		for _, old := range before.Drives {
			for _, now := range after.Drives {
				if old.Root == now.Root && old.ID == now.ID {
					sameVolume[now.Root] = true
				}
			}
		}
	}
	driveChanged := func(item *record) bool {
		return sameVolume != nil && item != nil && item.Path != "" && !sameVolume[volumeRoot(item.Path)]
	}
	var changes []change
	omitted := 0
	appendChange := func(key rowKey, old, fresh *record) {
		if key.Series == subfolders && (!before.Subfolders || !after.Subfolders) {
			return
		}
		a, b := int64(0), int64(0)
		if old != nil {
			a = old.Bytes
		}
		if fresh != nil {
			b = fresh.Bytes
		}
		if a < 0 || b < 0 || a == b {
			return
		}
		if (key.Series == candidates && before.Kind != after.Kind) ||
			(old != nil && old.Partial) || (fresh != nil && fresh.Partial) || driveChanged(old) || driveChanged(fresh) ||
			(old != nil && overlapsGap(old.Path, changedGaps)) ||
			(fresh != nil && overlapsGap(fresh.Path, changedGaps)) {
			omitted++
			return
		}
		delta, bounded := b-a, false
		if (key.Series == largeFiles || key.Series == subfolders) && (old == nil || fresh == nil) {
			bounded = true
			if old == nil {
				delta = max(0, b-fileFloor+1)
			} else {
				delta = -max(0, a-fileFloor+1)
			}
		}
		if delta != 0 {
			changes = append(changes, change{key.Series, old, fresh, delta, bounded})
		}
	}
	byLocation := make(map[rowKey]rowKey)
	for key, old := range older {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if fresh, ok := newer[key]; ok {
			appendChange(key, &old, &fresh)
		} else if location := comparisonLocation(old); location != "" {
			byLocation[rowKey{key.Series, location}] = key
		}
	}
	matched := make(map[rowKey]bool)
	for key, fresh := range newer {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if _, ok := older[key]; ok {
			continue
		}
		location := rowKey{key.Series, comparisonLocation(fresh)}
		if oldKey, ok := byLocation[location]; ok {
			old := older[oldKey]
			appendChange(key, &old, &fresh)
			matched[oldKey] = true
			delete(byLocation, location)
		} else {
			appendChange(key, nil, &fresh)
		}
	}
	for key, old := range older {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if _, ok := newer[key]; !ok && !matched[key] {
			appendChange(key, &old, nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	slices.SortFunc(changes, func(a, b change) int {
		if x, y := magnitude(a.Bytes), magnitude(b.Bytes); x != y {
			if x > y {
				return -1
			}
			return 1
		}
		x, y := a.item(), b.item()
		if x.Name != y.Name {
			return strings.Compare(x.Name, y.Name)
		}
		if x.Series != y.Series {
			return strings.Compare(x.Series, y.Series)
		}
		return strings.Compare(x.Key, y.Key)
	})
	return changes, omitted, ctx.Err()
}

func comparisonLocation(item record) string {
	if item.Path != "" {
		return normalizedKey(item.Path)
	}
	if item.Series == containers {
		return strings.ToLower(item.Name)
	}
	return ""
}

func overlapsGap(path string, gaps map[string]bool) bool {
	if path == "" || len(gaps) == 0 {
		return false
	}
	path = normalizedKey(path)
	for gap := range gaps {
		if path == gap || strings.HasPrefix(path, gap+"\\") || strings.HasPrefix(gap, path+"\\") {
			return true
		}
	}
	return false
}

func normalizedKey(path string) string {
	return strings.TrimRight(strings.ToLower(filepath.Clean(path)), "\\")
}

type recordFilter struct {
	path  string
	words []string
}

func parseFilter(query string) recordFilter {
	query = strings.ToLower(strings.TrimSpace(query))
	if path, ok := strings.CutPrefix(query, "path:"); ok && filepath.IsAbs(strings.TrimSpace(path)) {
		return recordFilter{path: normalizedKey(strings.TrimSpace(path))}
	}
	return recordFilter{words: strings.Fields(query)}
}

func (f recordFilter) matches(item record, detail string) bool {
	if f.path != "" {
		path := normalizedKey(item.Path)
		return path == f.path || strings.HasPrefix(path, f.path+"\\")
	}
	if len(f.words) == 0 {
		return true
	}
	haystack := strings.ToLower(item.Name + " " + item.Path + " " + item.Kind + " " + detail)
	for _, term := range f.words {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func magnitude(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func sizeText(value int64) string {
	if value < 0 {
		return "—"
	}
	for _, unit := range []struct {
		size int64
		name string
	}{{1 << 40, "TiB"}, {1 << 30, "GiB"}, {mib, "MiB"}, {1 << 10, "KiB"}} {
		if value >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(value)/float64(unit.size), unit.name)
		}
	}
	return fmt.Sprintf("%d B", value)
}

func deltaText(value int64, bounded bool) string {
	if value == 0 {
		return "0 B"
	}
	if bounded {
		arrow := " ↑"
		if value < 0 {
			arrow = " ↓"
		}
		return "≥ " + sizeText(magnitude(value)) + arrow
	}
	sign := "+"
	if value < 0 {
		sign = "−"
	}
	return sign + sizeText(magnitude(value))
}
