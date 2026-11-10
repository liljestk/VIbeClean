// Package store keeps capture history in a local SQLite index.
//
// Each reading is stored as a version that opens at the capture that first
// saw it and closes at the capture that replaced it, so unchanged readings
// cost nothing and a comparison reads only versions that change between its
// two endpoints.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Registers the pure-Go SQLite driver.

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/volume"
)

const (
	schemaVersion   = 3
	retention       = 120 * 24 * time.Hour
	snapshotFields  = "id, scope, label, captured, coverage"
	latestSnapshot  = `SELECT ` + snapshotFields + ` FROM snapshots WHERE scope = ? ORDER BY id DESC LIMIT 1`
	staleCapture    = "a newer capture finished first"
	legacyQuickKeys = "files-v1|quick review|%"
)

// Index is an open capture history.
type Index struct {
	db *sql.DB
}

// DefaultPath returns %LOCALAPPDATA%\SpaceReview\index.sqlite3.
func DefaultPath() (string, error) {
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

// Open opens or creates the index at a local path and upgrades its schema.
func Open(ctx context.Context, path string) (_ *Index, err error) {
	started := time.Now()
	stage, version := "prepare_path", 0
	var journal string
	defer func() {
		diag.Duration(ctx, "index.open", started, err, slog.String("stage", stage),
			slog.Int("prior_schema", version), slog.String("journal", journal))
	}()
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, `\\`) {
		return nil, model.InvalidArgument("choose a local path for the SQLite index")
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
	fail := func(err error) (*Index, error) {
		_ = db.Close()
		return nil, err
	}
	stage = "check_schema"
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > schemaVersion {
		return fail(model.InvalidArgument("the index was created by a newer version of Space Review"))
	}
	if version < schemaVersion {
		stage = "migrate"
		if err = migrate(ctx, db, version); err != nil {
			return fail(err)
		}
	}
	stage = "journal_mode"
	if journal, err = enableWAL(ctx, db, path); err != nil {
		return fail(err)
	}
	if journal == "wal" {
		// Readers keep their own connection while a capture is being saved.
		db.SetMaxOpenConns(2)
	}
	index := &Index{db: db}
	if adoptErr := index.adoptQuickHistory(ctx); adoptErr != nil {
		diag.Log(ctx, slog.LevelWarn, "index.adopt_quick_failed", slog.String("error_kind", diag.ErrorKind(adoptErr)))
	}
	return index, nil
}

func migrate(ctx context.Context, db *sql.DB, version int) error {
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
	`
	create := ""
	if version == 0 {
		create = schema
	}
	_, err := db.ExecContext(ctx, "BEGIN IMMEDIATE;"+create+upgrade+inventorySchema+"PRAGMA user_version = 3;COMMIT;")
	return err
}

func enableWAL(ctx context.Context, db *sql.DB, path string) (string, error) {
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return "", err
	}
	if journal == "wal" || !volume.IsFixed(model.VolumeRoot(path)) {
		return journal, nil
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		diag.Log(ctx, slog.LevelWarn, "index.wal_unavailable", slog.String("error_kind", diag.ErrorKind(err)))
		return journal, nil
	}
	return mode, nil
}

// Close closes the index.
func (i *Index) Close() error { return i.db.Close() }

// adoptQuickHistory moves the most recent Quick review history, which older
// versions keyed by the set of fixed drives, to the drive-independent key.
func (i *Index) adoptQuickHistory(ctx context.Context) (err error) {
	key := model.ScopeKey(model.QuickScope, nil)
	var pending bool
	if err := i.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM snapshots WHERE scope = ?)
		AND EXISTS (SELECT 1 FROM snapshots WHERE scope LIKE ?)`, key, legacyQuickKeys).Scan(&pending); err != nil || !pending {
		return err
	}
	started := time.Now()
	defer func() { diag.Duration(ctx, "index.adopt_quick", started, err) }()
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Readings move first: the first statement takes the writer lock, and the
	// snapshot rows still name the legacy scope until the second statement.
	const legacy = `(SELECT scope FROM snapshots WHERE scope LIKE ? ORDER BY id DESC LIMIT 1)`
	const unclaimed = `NOT EXISTS (SELECT 1 FROM snapshots WHERE scope = ?)`
	for _, table := range []string{"readings", "snapshots"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET scope = ? WHERE scope = `+legacy+` AND `+unclaimed,
			key, legacyQuickKeys, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type saveStats struct {
	encoded, writerWait, readActive, writeVersions, commit time.Duration
	reused, closed, added                                  int
}

// Save stores a capture in one transaction and sets its ID. Unchanged readings
// reuse their existing versions. A capture that another capture of the same
// scope overtook fails with a conflict.
func (i *Index) Save(ctx context.Context, s *model.Snapshot) error {
	return i.SaveWithInventory(ctx, s, nil)
}

// SaveWithInventory publishes the current inventory and its capture together.
// next returns io.EOF when the last entry has been read.
func (i *Index) SaveWithInventory(ctx context.Context, s *model.Snapshot, inventoryNext func() (model.InventoryEntry, error)) (err error) {
	started := time.Now()
	stage := "encode"
	var stats saveStats
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		diag.Duration(ctx, "index.save", started, err, slog.String("stage", stage),
			slog.Int64("capture_id", s.ID), slog.Int("readings", len(s.Records)),
			slog.Int("versions_reused", stats.reused), slog.Int("versions_closed", stats.closed), slog.Int("versions_added", stats.added),
			slog.Float64("encode_ms", diag.Milliseconds(stats.encoded)), slog.Float64("writer_wait_ms", diag.Milliseconds(stats.writerWait)),
			slog.Float64("read_active_ms", diag.Milliseconds(stats.readActive)), slog.Float64("write_versions_ms", diag.Milliseconds(stats.writeVersions)),
			slog.Float64("commit_ms", diag.Milliseconds(stats.commit)))
	}()
	if s.Captured == 0 {
		s.Captured = time.Now().UnixMilli()
	}
	s.Coverage.Inventory = inventoryNext != nil
	meta, err := json.Marshal(s.Coverage)
	if err != nil {
		return err
	}
	next := make(map[model.RowKey]string, len(s.Records))
	for _, item := range s.Records {
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		next[model.RowKey{Series: item.Series, Key: item.Key}] = string(raw)
	}
	stats.encoded = time.Since(started)
	waitStarted := time.Now()
	stage = "begin"
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stage = "insert_snapshot"
	id, err := insertSnapshot(ctx, tx, s, string(meta))
	stats.writerWait = time.Since(waitStarted)
	if err != nil {
		return err
	}
	stage = "write_versions"
	if err = writeVersions(ctx, tx, s.Scope, id, next, &stats); err != nil {
		return err
	}
	stage = "inventory"
	if err = replaceInventory(ctx, tx, s.Scope, id, inventoryNext); err != nil {
		return err
	}
	stage = "retention"
	if err = expire(ctx, tx, s); err != nil {
		return err
	}
	stage = "commit"
	phaseStarted := time.Now()
	err = tx.Commit()
	stats.commit = time.Since(phaseStarted)
	if err != nil {
		return err
	}
	s.ID = id
	return nil
}

// insertSnapshot takes the writer lock before any read. A capture that
// finished after this one makes it stale. A saved time still ahead of the
// clock means the clock moved back, which must not block every later save.
func insertSnapshot(ctx context.Context, tx *sql.Tx, s *model.Snapshot, meta string) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots(scope, label, captured, coverage)
		SELECT ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM snapshots WHERE scope = ? AND captured > ? AND captured <= ?)
	`, s.Scope, s.Label, s.Captured, meta, s.Scope, s.Captured, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return 0, model.Conflict(staleCapture)
	}
	return result.LastInsertId()
}

func writeVersions(ctx context.Context, tx *sql.Tx, scope string, id int64, next map[model.RowKey]string, stats *saveStats) error {
	phaseStarted := time.Now()
	active, err := readValues(ctx, tx, `SELECT series, item_key, value FROM readings
		WHERE scope = ? AND until IS NULL`, scope)
	stats.readActive = time.Since(phaseStarted)
	if err != nil {
		return err
	}
	phaseStarted = time.Now()
	defer func() { stats.writeVersions = time.Since(phaseStarted) }()
	closeRow, err := tx.PrepareContext(ctx, `UPDATE readings SET until = ?
		WHERE scope = ? AND series = ? AND item_key = ? AND until IS NULL`)
	if err != nil {
		return err
	}
	defer closeRow.Close()
	for key, previous := range active {
		if value, ok := next[key]; ok && value == previous {
			delete(next, key)
			stats.reused++
			continue
		}
		if _, err := closeRow.ExecContext(ctx, id, scope, key.Series, key.Key); err != nil {
			return err
		}
		stats.closed++
	}
	addRow, err := tx.PrepareContext(ctx, `
		INSERT INTO readings(scope, series, item_key, first, value) VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer addRow.Close()
	for key, value := range next {
		if _, err := addRow.ExecContext(ctx, scope, key.Series, key.Key, id, value); err != nil {
			return err
		}
		stats.added++
	}
	return nil
}

// expire keeps the retention window and one baseline before it. Only captures
// older than every capture inside the window expire, so a capture stamped by a
// wrong clock cannot remove the history saved before it.
func expire(ctx context.Context, tx *sql.Tx, s *model.Snapshot) error {
	var anchor int64
	cutoff := s.Captured - retention.Milliseconds()
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM snapshots WHERE scope = ? AND captured < ?
		AND id < (SELECT MIN(id) FROM snapshots WHERE scope = ? AND captured >= ?)`,
		s.Scope, cutoff, s.Scope, cutoff).Scan(&anchor)
	if err != nil || anchor == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE scope = ? AND id < ?`, s.Scope, anchor); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM readings WHERE scope = ? AND until <= ?`, s.Scope, anchor)
	return err
}

func readValues(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[model.RowKey]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[model.RowKey]string)
	for rows.Next() {
		var key model.RowKey
		var value string
		if err := rows.Scan(&key.Series, &key.Key, &value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, rows.Err()
}

func loadSnapshot(row interface{ Scan(...any) error }) (model.Snapshot, error) {
	var s model.Snapshot
	var raw string
	if err := row.Scan(&s.ID, &s.Scope, &s.Label, &s.Captured, &raw); err != nil {
		return s, err
	}
	return s, json.Unmarshal([]byte(raw), &s.Coverage)
}

func noCapture(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return model.WithCause(model.NotFound("no saved capture for this location. Capture it first"), err)
	}
	return err
}

// List returns the latest capture of every scope, newest first, without records.
func (i *Index) List(ctx context.Context) (entries []model.Snapshot, err error) {
	started := time.Now()
	defer func() { diag.Duration(ctx, "index.list", started, err, slog.Int("scopes", len(entries))) }()
	return i.listSnapshots(ctx, `SELECT `+snapshotFields+` FROM snapshots
		WHERE id IN (SELECT MAX(id) FROM snapshots GROUP BY scope)
		ORDER BY captured DESC, id DESC`)
}

// History returns capture metadata for one scope, newest saved capture first.
func (i *Index) History(ctx context.Context, scope string) ([]model.Snapshot, error) {
	return i.listSnapshots(ctx, `SELECT `+snapshotFields+` FROM snapshots WHERE scope = ? ORDER BY id DESC`, scope)
}

func (i *Index) listSnapshots(ctx context.Context, query string, args ...any) ([]model.Snapshot, error) {
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []model.Snapshot
	for rows.Next() {
		s, err := loadSnapshot(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, s)
	}
	return entries, rows.Err()
}

// Latest returns the latest capture of a scope with all of its records.
func (i *Index) Latest(ctx context.Context, scope string) (s model.Snapshot, err error) {
	started := time.Now()
	defer func() {
		diag.Duration(ctx, "index.latest", started, err, slog.Int64("capture_id", s.ID), slog.Int("readings", len(s.Records)))
	}()
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	s, err = loadSnapshot(tx.QueryRowContext(ctx, latestSnapshot, scope))
	if err != nil {
		return model.Snapshot{}, noCapture(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT value FROM readings WHERE scope = ? AND until IS NULL`, s.Scope)
	if err != nil {
		return model.Snapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return model.Snapshot{}, err
		}
		var item model.Record
		if err := json.Unmarshal(value, &item); err != nil {
			return model.Snapshot{}, err
		}
		s.Records = append(s.Records, item)
	}
	if err := rows.Err(); err != nil {
		return model.Snapshot{}, err
	}
	return s, tx.Commit()
}

func readRecords(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[model.RowKey]model.Record, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[model.RowKey]model.Record)
	for rows.Next() {
		var key model.RowKey
		var value []byte
		if err := rows.Scan(&key.Series, &key.Key, &value); err != nil {
			return nil, err
		}
		var item model.Record
		if err := json.Unmarshal(value, &item); err != nil {
			return nil, err
		}
		items[key] = item
	}
	return items, rows.Err()
}

// Compare ranks changes between the latest capture of a scope and a baseline:
// the previous capture when days is 0, otherwise the nearest capture at or
// before the cutoff. Without a baseline, the result has a zero Baseline.
func (i *Index) Compare(ctx context.Context, scope string, days int) (model.Comparison, error) {
	return i.compare(ctx, scope, days, 0, 0)
}

// ComparePair compares two particular captures of one scope, in capture order.
func (i *Index) ComparePair(ctx context.Context, scope string, from, to int64) (model.Comparison, error) {
	if from <= 0 || from >= to {
		return model.Comparison{}, model.InvalidArgument("choose an earlier and a later capture")
	}
	return i.compare(ctx, scope, 0, from, to)
}

func (i *Index) compare(ctx context.Context, scope string, days int, from, to int64) (result model.Comparison, err error) {
	started := time.Now()
	stage := "begin"
	var earlierRows, laterRows int
	var readTime, matchTime time.Duration
	defer func() {
		diag.Duration(ctx, "index.compare", started, err, slog.String("stage", stage), slog.Int("days", days),
			slog.Int64("baseline_id", result.Baseline.ID), slog.Int64("capture_id", result.Latest.ID),
			slog.Int("baseline_versions", earlierRows), slog.Int("latest_versions", laterRows),
			slog.Int("changes", len(result.Changes)), slog.Int("omitted", result.Omitted), slog.Bool("short_window", result.ShortWindow),
			slog.Float64("read_ms", diag.Milliseconds(readTime)), slog.Float64("match_ms", diag.Milliseconds(matchTime)))
	}()
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	const byID = `SELECT ` + snapshotFields + ` FROM snapshots WHERE scope = ? AND id = ?`
	stage = "read_latest"
	if to == 0 {
		result.Latest, err = loadSnapshot(tx.QueryRowContext(ctx, latestSnapshot, scope))
	} else {
		result.Latest, err = loadSnapshot(tx.QueryRowContext(ctx, byID, scope, to))
	}
	if err != nil {
		return result, noCapture(err)
	}
	stage = "read_baseline"
	if from == 0 {
		result.Baseline, result.ShortWindow, err = baseline(ctx, tx, scope, result.Latest, days)
		if errors.Is(err, sql.ErrNoRows) {
			return result, tx.Commit()
		}
	} else {
		result.Baseline, err = loadSnapshot(tx.QueryRowContext(ctx, byID, scope, from))
	}
	if err != nil {
		return result, noCapture(err)
	}
	if !slices.Equal(result.Baseline.Coverage.Exclusions, result.Latest.Coverage.Exclusions) {
		return result, model.InvalidArgument("these scans used different exclusions. Choose scans with matching exclusions")
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

func baseline(ctx context.Context, tx *sql.Tx, scope string, latest model.Snapshot, days int) (model.Snapshot, bool, error) {
	paths, err := json.Marshal(append([]string{}, latest.Coverage.Exclusions...))
	if err != nil {
		return model.Snapshot{}, false, err
	}
	const matching = `WHERE scope = ? AND id < ? AND COALESCE(json_extract(coverage, '$.exclusions'), '[]') = ?`
	if days == 0 {
		s, err := loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots `+
			matching+` ORDER BY id DESC LIMIT 1`, scope, latest.ID, string(paths)))
		return s, false, err
	}
	cutoff := latest.Captured - (time.Duration(days) * 24 * time.Hour).Milliseconds()
	s, err := loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots `+
		matching+` AND captured <= ? ORDER BY id DESC LIMIT 1`, scope, latest.ID, string(paths), cutoff))
	if !errors.Is(err, sql.ErrNoRows) {
		return s, false, err
	}
	// The window reaches further back than the history: use the oldest capture.
	s, err = loadSnapshot(tx.QueryRowContext(ctx, `SELECT `+snapshotFields+` FROM snapshots `+
		matching+` ORDER BY id LIMIT 1`, scope, latest.ID, string(paths)))
	return s, true, err
}
