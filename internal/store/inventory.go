package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

const inventorySchema = `
	CREATE TABLE IF NOT EXISTS inventory (
		capture INTEGER NOT NULL,
		path TEXT NOT NULL,
		path_key TEXT NOT NULL,
		parent_key TEXT NOT NULL,
		name_key TEXT NOT NULL,
		extension TEXT NOT NULL,
		kind TEXT NOT NULL,
		bytes INTEGER NOT NULL,
		length INTEGER NOT NULL,
		modified INTEGER NOT NULL,
		files INTEGER NOT NULL,
		directory INTEGER NOT NULL,
		offline INTEGER NOT NULL,
		shared INTEGER NOT NULL,
		PRIMARY KEY (capture, path)
	) WITHOUT ROWID;
	CREATE INDEX IF NOT EXISTS inventory_children ON inventory(capture, parent_key, bytes DESC, path_key);
	CREATE INDEX IF NOT EXISTS inventory_files ON inventory(capture, directory, bytes DESC, path_key);
	CREATE INDEX IF NOT EXISTS inventory_lengths ON inventory(capture, length) WHERE directory = 0 AND offline = 0;
`

// replaceInventory drops the preceding detail for this scope and writes the
// new detail in bounded batches. The caller commits it with the summary.
func replaceInventory(ctx context.Context, tx *sql.Tx, scope string, id int64, next func() (model.InventoryEntry, error)) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM inventory WHERE capture IN
		(SELECT id FROM snapshots WHERE scope = ? AND id < ?)`, scope, id); err != nil || next == nil {
		return err
	}
	const batchSize, columns = 64, 14
	const fields = `INSERT INTO inventory(capture, path, path_key, parent_key, name_key, extension,
		kind, bytes, length, modified, files, directory, offline, shared) VALUES `
	tuple := "(" + strings.Repeat("?,", columns-1) + "?)"
	full, err := tx.PrepareContext(ctx, fields+strings.TrimSuffix(strings.Repeat(tuple+",", batchSize), ","))
	if err != nil {
		return err
	}
	defer full.Close()
	args := make([]any, 0, batchSize*columns)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := next()
		if errors.Is(err, io.EOF) {
			if len(args) > 0 {
				_, err = tx.ExecContext(ctx, fields+strings.TrimSuffix(strings.Repeat(tuple+",", len(args)/columns), ","), args...)
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		args = append(args, id, item.Path, model.NormalizedKey(item.Path), inventoryKey(item.Parent),
			strings.ToLower(filepath.Base(item.Path)), strings.ToLower(filepath.Ext(item.Path)), item.Kind,
			item.Bytes, item.Length, item.Modified, item.Files, item.Directory, item.Offline, item.Shared)
		if len(args) == cap(args) {
			if _, err := full.ExecContext(ctx, args...); err != nil {
				return err
			}
			args = args[:0]
		}
	}
}

// DuplicateCandidates copies the metadata needed for a content scan in
// length order. Its caller can finish the read before opening any files.
func (i *Index) DuplicateCandidates(ctx context.Context, id int64, folder string, emit func(model.InventoryEntry) error) (int64, error) {
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := requireInventory(ctx, tx, id); err != nil {
		return 0, err
	}
	where, args := inventoryWhere(service.InventoryQuery{CaptureID: id, Files: true, Parent: folder}, 0)
	var offline int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory`+where+` AND offline = 1`, args...).Scan(&offline); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `WITH candidates AS (
		SELECT path, path_key, length, modified FROM inventory`+where+` AND offline = 0 AND length > 0
	), repeated AS (SELECT length FROM candidates GROUP BY length HAVING COUNT(*) > 1)
	SELECT path, length, modified FROM candidates JOIN repeated USING(length) ORDER BY length, path_key, path`, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item model.InventoryEntry
		if err := rows.Scan(&item.Path, &item.Length, &item.Modified); err != nil {
			return 0, err
		}
		if err := emit(item); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return offline, tx.Commit()
}

func inventoryKey(path string) string {
	if path == "" {
		return ""
	}
	return model.NormalizedKey(path)
}

func requireInventory(ctx context.Context, tx *sql.Tx, id int64) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM snapshots AS s WHERE s.id = ? AND json_extract(s.coverage, '$.inventory') = 1
		AND NOT EXISTS (SELECT 1 FROM snapshots AS newer WHERE newer.scope = s.scope AND newer.id > s.id))`, id).Scan(&present); err != nil {
		return err
	}
	if !present {
		return model.NotFound("scan this location to open its current file inventory")
	}
	return nil
}

// Inventory returns a consistent page and total from the current detail.
func (i *Index) Inventory(ctx context.Context, q service.InventoryQuery) (service.InventoryPage, error) {
	var page service.InventoryPage
	total, err := i.readInventory(ctx, q, true, func(item model.Record) error {
		page.Items = append(page.Items, item)
		return nil
	})
	page.Total = total
	return page, err
}

func (i *Index) StreamInventory(ctx context.Context, q service.InventoryQuery, emit func(model.Record) error) error {
	_, err := i.readInventory(ctx, q, false, emit)
	return err
}

func (i *Index) readInventory(ctx context.Context, q service.InventoryQuery, paged bool, emit func(model.Record) error) (int, error) {
	columns := [...]string{"name_key", "bytes", "kind COLLATE NOCASE", "modified"}
	if q.CaptureID <= 0 || (paged && (q.Limit <= 0 || q.Limit > service.InventoryPageSize || q.Offset < 0)) ||
		q.MinimumBytes < 0 || q.SortColumn < 0 || q.SortColumn >= len(columns) {
		return 0, model.InvalidArgument("invalid file inventory request")
	}
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := requireInventory(ctx, tx, q.CaptureID); err != nil {
		return 0, err
	}
	var labels int
	if q.Removal != "" {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_extract(coverage, '$.labels'), 0) FROM snapshots WHERE id = ?`,
			q.CaptureID).Scan(&labels); err != nil {
			return 0, err
		}
	}
	where, args := inventoryWhere(q, labels)
	order := " DESC"
	if q.Ascending {
		order = " ASC"
	}
	query := `SELECT path, kind, bytes, length, modified, files, directory, offline, shared FROM inventory` + where +
		` ORDER BY ` + columns[q.SortColumn] + order + `, path_key, path`
	var total int
	if paged {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory`+where, args...).Scan(&total); err != nil {
			return 0, err
		}
		query += ` LIMIT ? OFFSET ?`
		args = append(args, q.Limit, q.Offset)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item model.Record
		var offline, shared bool
		if err := rows.Scan(&item.Path, &item.Kind, &item.Bytes, &item.Logical, &item.Modified, &item.Files, &item.Directory, &offline, &shared); err != nil {
			return 0, err
		}
		item.Key, item.Name, item.Series = item.Path, filepath.Base(item.Path), model.Folders
		if !item.Directory {
			item.Series = model.LargeFiles
		}
		switch {
		case shared:
			item.Detail = "Another hard link is already counted in this scan."
		case offline:
			item.Detail = "Cloud file with content that may be online-only. Size shows local allocation."
		}
		if err := emit(item); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

func inventoryWhere(q service.InventoryQuery, labels int) (string, []any) {
	where, args := []string{"capture = ?"}, []any{q.CaptureID}
	add := func(condition string, values ...any) {
		where, args = append(where, condition), append(args, values...)
	}
	under := func(key string) {
		add("(path_key = ? OR instr(path_key, ?) = 1)", key, key+`\`)
	}
	if q.Files {
		add("directory = 0")
		if q.Parent != "" {
			under(inventoryKey(q.Parent))
		}
	} else {
		add("parent_key = ?", inventoryKey(q.Parent))
	}
	if q.Within != "" {
		under(inventoryKey(q.Within))
	}
	if q.Extension != "" {
		ext := "." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(q.Extension)), ".")
		add("directory = 0 AND extension = ?", ext)
	}
	if q.MinimumBytes > 0 {
		add("bytes >= ?", q.MinimumBytes)
	}
	if q.ModifiedSince != 0 {
		add("modified >= ?", q.ModifiedSince)
	}
	if q.Removal != "" {
		kinds, except := model.RemovalKinds(q.Removal, labels)
		operator := " IN "
		if except {
			operator = " NOT IN "
		}
		// SQLite treats an empty IN list as a false condition.
		where = append(where, "kind"+operator+"("+strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")+")")
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	filter := model.ParseFilter(q.Query)
	if folder := filter.Folder(); folder != "" {
		under(folder)
	}
	for _, word := range filter.Terms() {
		add("(instr(path_key, ?) > 0 OR instr(lower(kind), ?) > 0)", word, word)
	}
	return " WHERE " + strings.Join(where, " AND "), args
}
