package ui

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

func (u *ui) showFileRows(rows []displayRow, query, empty string) {
	cancelGUIRead(&u.fileCheck)
	u.filePane.missingPaths, u.folderMissing = nil, false
	u.filePane.show(rows, query, empty)
	u.checkFilePaths()
}

type filePresence struct {
	missing       map[string]bool
	folderMissing bool
}

func (u *ui) checkFilePaths() {
	backend := u.backend
	if backend == nil || u.view != viewFiles || u.files.ID == 0 || u.fileLoad != nil || u.inventoryNotice != "" {
		return
	}
	folder := u.files.Label
	if u.usesInventory() && u.fileParent != "" {
		folder = u.fileParent
	}
	pane := &u.filePane
	rows, drives := pane.items, u.files.Coverage.Drives
	readForGUI(u, &u.fileCheck, "gui.file_presence", func(ctx context.Context) (filePresence, error) {
		return readFilePresence(ctx, backend, drives, folder, rows)
	}, func(presence filePresence, err error) {
		if err != nil || (u.folderMissing == presence.folderMissing && maps.Equal(pane.missingPaths, presence.missing)) {
			return
		}
		pane.missingPaths, u.folderMissing = presence.missing, presence.folderMissing
		query := ""
		if !u.usesInventory() {
			query = u.fileFilter.text()
		}
		pane.show(pane.items, query, pane.emptyText)
	})
}

func readFilePresence(ctx context.Context, backend service.Backend, captured []model.DriveState, folder string, rows []displayRow) (filePresence, error) {
	missing := make(map[string]bool)
	folderMissing := filePathMissing(folder)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return filePresence{}, err
		}
		if path := row.Item.Path; filePathMissing(path) {
			missing[path] = true
		}
	}
	if len(missing) == 0 && !folderMissing {
		return filePresence{}, nil
	}
	// A missing path on an unavailable or replaced drive is inconclusive.
	current, err := backend.Drives(ctx)
	if err != nil {
		return filePresence{}, err
	}
	mounted := make(map[string]bool, len(current))
	for _, drive := range current {
		old := slices.IndexFunc(captured, func(saved model.DriveState) bool { return saved.Root == drive.Root })
		mounted[drive.Root] = old < 0 || captured[old].ID == "" || captured[old].ID == drive.ID
	}
	for path := range missing {
		if !mounted[model.VolumeRoot(path)] {
			delete(missing, path)
		}
	}
	return filePresence{missing: missing, folderMissing: folderMissing && mounted[model.VolumeRoot(folder)]}, nil
}

func filePathMissing(path string) bool {
	if !filepath.IsAbs(path) || model.VolumeRoot(path) == "" {
		return false
	}
	_, err := os.Lstat(path)
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
