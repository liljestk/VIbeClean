package scan

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/volume"
)

// target is one location in a capture. A split target becomes one reading per
// entry inside it; any other target is a single file or folder.
type target struct {
	Path  string
	Kind  string
	Split bool
}

// quickFinder collects the locations Quick review measures: AI tool and app
// data, scratch folders, Docker disks and backups near the top of each drive
// and the user's documents.
type quickFinder struct {
	ctx    context.Context
	reader *directoryReader
	meta   *model.Coverage
	paths  []target
}

func (q *quickFinder) add(path, kind string, split bool) {
	if path != "" {
		q.paths = append(q.paths, target{path, kind, split})
	}
}

func quickTargets(ctx context.Context, reader *directoryReader, roots []string, meta *model.Coverage, extra []string) []target {
	q := &quickFinder{ctx: ctx, reader: reader, meta: meta}
	home, _ := os.UserHomeDir()
	q.addToolFolders(home, os.Getenv("LOCALAPPDATA"), os.Getenv("APPDATA"))
	for _, root := range roots {
		q.add(filepath.Join(root, "tmp"), model.ScratchKind, true)
		q.add(filepath.Join(root, "temp"), model.ScratchKind, true)
	}
	for _, disk := range extra {
		q.add(disk, model.DockerKind, false)
	}
	for _, root := range roots {
		q.findBackups(root, false)
	}
	if home != "" {
		q.findBackups(home, false)
		q.findBackups(filepath.Join(home, "Documents"), true)
		oneDrive := os.Getenv("OneDrive")
		if oneDrive == "" {
			oneDrive = filepath.Join(home, "OneDrive")
		}
		q.findBackups(filepath.Join(oneDrive, "Documents"), true)
	}
	return selectTargets(reader, meta, q.paths)
}

func (q *quickFinder) addToolFolders(home, local, roaming string) {
	for _, pair := range [][3]string{{"CODEX_HOME", ".codex", model.AppKind}, {"CLAUDE_CONFIG_DIR", ".claude", model.AppKind}} {
		path := os.Getenv(pair[0])
		if path == "" && home != "" {
			path = filepath.Join(home, pair[1])
		}
		q.add(path, pair[2], true)
	}
	if home != "" {
		q.add(filepath.Join(home, "Documents", "Codex"), model.ProjectKind, true)
	}
	if local != "" {
		q.add(filepath.Join(local, "Stugu", "tooling"), model.ProjectKind, true)
		q.add(filepath.Join(local, "OpenAI", "Codex"), model.AppKind, true)
		for _, name := range []string{"Claude-Data", "AnthropicClaude", "Codex", "Claude"} {
			q.add(filepath.Join(local, name), model.AppKind, true)
		}
		q.add(filepath.Join(local, "Temp"), model.TempKind, true)
		q.addPackages(filepath.Join(local, "Packages"))
	}
	if roaming != "" {
		for _, app := range []string{"Codex", "Claude"} {
			q.add(filepath.Join(roaming, app), model.AppKind, true)
		}
	}
}

// addPackages finds packaged Codex and Claude apps.
func (q *quickFinder) addPackages(packages string) {
	if _, err := LocalFolder(packages); err != nil {
		q.reader.addGap(q.meta, packages, err)
		return
	}
	err := q.reader.entries(q.ctx, packages, 0, func(entry dirEntry) {
		lower := strings.ToLower(entry.Name)
		if entry.directory() && !entry.link() && (strings.HasPrefix(lower, "openai.codex") ||
			strings.HasPrefix(lower, "claude_") || strings.HasPrefix(lower, "anthropic")) {
			q.add(filepath.Join(packages, entry.Name), model.AppKind, true)
		}
	})
	q.reader.addGap(q.meta, packages, err)
}

// findBackups adds backup and recovery entries in root and, when asked, in
// its visible subfolders.
func (q *quickFinder) findBackups(root string, includeChildren bool) {
	if q.ctx.Err() != nil {
		return
	}
	if _, err := LocalFolder(root); err != nil {
		q.reader.addGap(q.meta, root, err)
		return
	}
	var children []string
	at := placeOf(root, model.FilesKind)
	err := q.reader.entries(q.ctx, root, 0, func(entry dirEntry) {
		if entry.link() {
			return
		}
		path := filepath.Join(root, entry.Name)
		kind := classify(entry.Name, at, entry.directory()).kind
		if kind == model.BackupKind || kind == model.RecoveryKind {
			q.add(path, kind, false)
		} else if includeChildren && entry.directory() && !strings.HasPrefix(entry.Name, ".") {
			children = append(children, path)
		}
	})
	q.reader.addGap(q.meta, root, err)
	for _, child := range children {
		q.findBackups(child, false)
	}
}

// selectTargets keeps reachable targets on fixed drives, drops duplicates and
// drops targets inside another target.
func selectTargets(reader *directoryReader, meta *model.Coverage, paths []target) []target {
	var selected []target
	keys := make(map[string]bool)
	for _, t := range paths {
		path, err := filepath.Abs(t.Path)
		if err != nil || model.VolumeRoot(path) == "" || !volume.IsFixed(model.VolumeRoot(path)) {
			continue
		}
		folder := path
		if !t.Split {
			folder = filepath.Dir(path)
		}
		if _, err := LocalFolder(folder); err != nil {
			reader.addGap(meta, folder, err)
			continue
		}
		key := model.NormalizedKey(path)
		if !keys[key] {
			keys[key] = true
			selected = append(selected, target{path, t.Kind, t.Split})
		}
	}
	slices.SortFunc(selected, func(a, b target) int {
		return strings.Compare(model.NormalizedKey(a.Path), model.NormalizedKey(b.Path))
	})
	return slices.DeleteFunc(selected, func(t target) bool { return insideOther(t.Path, keys) })
}

func insideOther(path string, keys map[string]bool) bool {
	for parent := filepath.Dir(path); parent != path; parent = filepath.Dir(parent) {
		if keys[model.NormalizedKey(parent)] {
			return true
		}
		if filepath.Dir(parent) == parent {
			return false
		}
	}
	return false
}
