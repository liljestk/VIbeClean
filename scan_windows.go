package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	attrsDirectory = uint32(0x10)
	attrsReparse   = uint32(0x400)
	attrsOffline   = uint32(0x441000)
	residentLimit  = int64(4096) // Longest file data an MFT record can hold.
	filesKind      = "Files"
	projectKind    = "Project files"
	appKind        = "App data"
	scratchKind    = "Scratch"
	backupKind     = "Backups"
	cacheKind      = "Cache & logs"
	checkoutKind   = "Checkouts"
	buildKind      = "Build output"
	recoveryKind   = "Windows recovery"
	dockerKind     = "Docker storage"
	systemKind     = "System files"
)

var (
	kernel32     = windows.NewLazySystemDLL("kernel32.dll")
	extendedList = kernel32.NewProc("GetFileInformationByHandleEx")
	storedSize   = kernel32.NewProc("GetCompressedFileSizeW")
	backupName   = regexp.MustCompile("backup|filehistory|windowsimagebackup|(?:^|[-_. ])(?:recover(?:y|ed|_all|[0-9]+)?|restore[d]?|rollback)(?:$|[-_. ])")
	backupExt    = regexp.MustCompile("\\.(?:bak[0-9]*|bkp|backup|hsvbak|dump)(?:$|\\.(?:zip|gz|zst)$)")
	archiveExt   = regexp.MustCompile("\\.(?:zip|7z|tar|gz|zst|rar|img|vhdx?)$")
)

type fileID struct {
	Low, High uint64
}

type dirEntry struct {
	Name     string
	ID       fileID
	Bytes    int64
	Length   int64
	Modified int64
	Attrs    uint32
	Tag      uint32
}

func (e dirEntry) directory() bool { return e.Attrs&attrsDirectory != 0 }
func (e dirEntry) link() bool {
	return e.Attrs&attrsReparse != 0 && e.Tag&0xffff0fff != 0x9000001a
}

type directoryReader struct {
	buffer       [64 * 1024]byte
	estimated    int64
	directories  int64
	batches      int64
	sizeLookups  int64
	sizeFailures int64
	gapKinds     map[string]int
}

func longPath(path string) string { return "\\\\?\\" + path }

// compressedSize returns the bytes a file stores on disk, including data kept
// by the Windows Overlay Filter. It reads metadata only and returns 0 on failure.
func (r *directoryReader) compressedSize(path string) int64 {
	r.sizeLookups++
	ptr, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		r.sizeFailures++
		return 0
	}
	var high uint32
	low, _, callErr := storedSize.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xffffffff && callErr != windows.ERROR_SUCCESS {
		r.sizeFailures++
		return 0
	}
	return max(0, int64(high)<<32|int64(uint32(low)))
}

func (r *directoryReader) entries(ctx context.Context, path string, namedFrom int64, consume func(dirEntry)) error {
	r.directories++
	ptr, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return err
	}
	// List metadata in batches without opening file contents or following a final link.
	handle, err := windows.CreateFile(ptr, windows.FILE_LIST_DIRECTORY, 7, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT|0x00100000, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	buf := r.buffer[:]
	for started := false; ; started = true {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.batches++
		ok, _, queryErr := extendedList.Call(uintptr(handle), 19, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if ok == 0 {
			if errors.Is(queryErr, windows.ERROR_NO_MORE_FILES) || errors.Is(queryErr, windows.ERROR_FILE_NOT_FOUND) {
				return nil
			}
			if !started && (errors.Is(queryErr, windows.ERROR_INVALID_PARAMETER) ||
				errors.Is(queryErr, windows.ERROR_NOT_SUPPORTED) || errors.Is(queryErr, windows.ERROR_INVALID_FUNCTION)) {
				return r.fallback(ctx, path, consume)
			}
			return queryErr
		}
		for offset := 0; ; {
			const header = 88 // FILE_ID_EXTD_DIR_INFO, before the UTF-16 name.
			if offset < 0 || offset+header > len(buf) {
				return diagnosticCode("invalid directory record offset")
			}
			entry := buf[offset:]
			step := int(binary.LittleEndian.Uint32(entry))
			nameLength := int(binary.LittleEndian.Uint32(entry[60:]))
			if nameLength%2 != 0 || nameLength > len(entry)-header {
				return diagnosticCode("invalid directory record name")
			}
			attrs := binary.LittleEndian.Uint32(entry[56:])
			size := max(0, int64(binary.LittleEndian.Uint64(entry[48:])))
			length := int64(binary.LittleEndian.Uint64(entry[40:]))
			name := ""
			decodeName := func() string {
				return windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&entry[header])), nameLength/2))
			}
			// Directory listings report zero allocation for WOF-compressed files
			// (CompactOS, compact /exe) because the data lives in an alternate stream.
			// Files this long cannot be resident in the MFT, so ask for their real size.
			if nameLength != 0 && size == 0 && length > residentLimit && attrs&(attrsDirectory|attrsOffline) == 0 {
				name = decodeName()
				size = r.compressedSize(filepath.Join(path, name))
			}
			if name == "" && nameLength != 0 && (attrs&attrsDirectory != 0 || size >= namedFrom) {
				name = decodeName()
			}
			if name != "." && name != ".." {
				consume(dirEntry{
					Name: name, Bytes: size, Attrs: attrs,
					ID:       fileID{binary.LittleEndian.Uint64(entry[72:]), binary.LittleEndian.Uint64(entry[80:])},
					Length:   length,
					Modified: filetimeMillis(binary.LittleEndian.Uint64(entry[24:])),
					Tag:      binary.LittleEndian.Uint32(entry[68:]),
				})
			}
			if step == 0 {
				break
			}
			if step < header+nameLength || step%8 != 0 {
				return diagnosticCode("invalid directory record length")
			}
			offset += step
		}
	}
}

func filetimeMillis(ticks uint64) int64 {
	if ticks <= 116444736000000000 {
		return 0
	}
	return int64((ticks - 116444736000000000) / 10000)
}

func (r *directoryReader) fallback(ctx context.Context, path string, consume func(dirEntry)) error {
	r.estimated++
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		ptr, err := windows.UTF16PtrFromString(longPath(filepath.Join(path, entry.Name())))
		if err != nil {
			return err
		}
		attrs, err := windows.GetFileAttributes(ptr)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		size := info.Size()
		if attrs&attrsOffline != 0 {
			size = 0
		}
		consume(dirEntry{Name: entry.Name(), Bytes: size, Length: info.Size(), Attrs: attrs, Modified: info.ModTime().UnixMilli()})
	}
	return nil
}

func driveRoots() []string {
	bits, _ := windows.GetLogicalDrives()
	var drives []string
	for index := range 26 {
		if bits&(1<<index) != 0 {
			root := fmt.Sprintf("%c:\\", 'A'+index)
			if isFixed(root) {
				drives = append(drives, root)
			}
		}
	}
	return drives
}

func isFixed(root string) bool {
	ptr, err := windows.UTF16PtrFromString(root)
	return err == nil && windows.GetDriveType(ptr) == 3
}

func diskState(roots []string) []driveState {
	var states []driveState
	for _, root := range roots {
		ptr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		var total, free uint64
		if err := windows.GetDiskFreeSpaceEx(ptr, nil, &total, &free); err != nil {
			continue
		}
		var volume [64]uint16
		id := strings.ToLower(root)
		if windows.GetVolumeNameForVolumeMountPoint(ptr, &volume[0], uint32(len(volume))) == nil {
			id = strings.ToLower(windows.UTF16ToString(volume[:]))
		}
		state := driveState{Root: root, ID: id, Total: int64(total), Free: int64(free)}
		// This query on a root directory handle is available without elevation on NTFS.
		handle, err := windows.CreateFile(ptr, 0, 7, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err == nil {
			var data [80]byte
			var count uint32
			if windows.DeviceIoControl(handle, 0x900f4, nil, 0, &data[0], uint32(len(data)), &count, nil) == nil && count >= 24 {
				state.Journal = binary.LittleEndian.Uint64(data[:])
				state.NextUSN = int64(binary.LittleEndian.Uint64(data[16:]))
			}
			windows.CloseHandle(handle)
		}
		states = append(states, state)
	}
	return states
}

func volumeRoot(path string) string {
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return ""
	}
	return strings.ToUpper(volume) + "\\"
}

func localFolder(path string) (string, error) {
	if len(path) == 2 && path[1] == ':' {
		path += "\\"
	}
	if !filepath.IsAbs(path) || volumeRoot(path) == "" {
		return "", errors.New("choose an absolute folder on a local fixed drive")
	}
	path = filepath.Clean(path)
	root := volumeRoot(path)
	if !isFixed(root) {
		return "", errors.New("choose a folder on a local fixed drive")
	}
	for current := path; !strings.EqualFold(current, root); current = filepath.Dir(current) {
		ptr, err := windows.UTF16PtrFromString(longPath(current))
		if err != nil {
			return "", err
		}
		var data windows.Win32finddata
		handle, err := windows.FindFirstFile(ptr, &data)
		if err != nil {
			return "", err
		}
		windows.FindClose(handle)
		e := dirEntry{Attrs: data.FileAttributes, Tag: data.Reserved0}
		if !e.directory() || e.link() {
			return "", errors.New("choose the folder's original location instead of a junction or link")
		}
	}
	return path, nil
}

type target struct {
	Path  string
	Kind  string
	Split bool
}

func classify(name, parent string, directory, atDriveRoot bool) string {
	lower := strings.ToLower(name)
	switch parent {
	case systemKind, recoveryKind, dockerKind, backupKind:
		return parent
	}
	if atDriveRoot && directory {
		switch lower {
		case "windows.old":
			return recoveryKind
		case "$recycle.bin", "program files", "program files (x86)", "programdata", "recovery",
			"system volume information", "windows", "windowsapps", "wpsystem":
			return systemKind
		}
	}
	if !directory {
		switch lower {
		case "docker_data.vhdx", "dockerdesktop.vhdx", "dockerdesktopvm.vhdx":
			return dockerKind
		}
		if backupExt.MatchString(lower) || (archiveExt.MatchString(lower) && backupName.MatchString(lower)) {
			return backupKind
		}
		if strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".log.gz") ||
			strings.HasSuffix(lower, ".log.1") || strings.HasSuffix(lower, ".dmp") {
			return cacheKind
		}
		return parent
	}
	if backupName.MatchString(lower) {
		return backupKind
	}
	switch lower {
	case "worktrees", "release-checkouts", "source-checkout", "checkouts":
		return checkoutKind
	case "cache", "caches", ".cache", "gpucache", "code cache", "localcache", "paste-cache",
		"shadercache", "dawncache", "logs", "log", "debug", "crashdumps":
		return cacheKind
	case "temp", "tmp", ".tmp":
		return scratchKind
	case "node_modules", "dist", "target", "coverage", "playwright-report", "test-results":
		if parent == filesKind || parent == projectKind || parent == checkoutKind || parent == scratchKind {
			return buildKind
		}
	}
	if strings.HasSuffix(lower, "-wt") || strings.HasSuffix(lower, "-worktree") {
		return checkoutKind
	}
	return parent
}

func isCandidate(kind string) bool {
	switch kind {
	case backupKind, cacheKind, scratchKind, checkoutKind, buildKind, recoveryKind:
		return true
	}
	return false
}

func kindNote(kind string) string {
	switch kind {
	case backupKind:
		return "Backup or recovery data. Check which copies you still need."
	case cacheKind:
		return "Cache or diagnostic files. Check whether the owning app is using them."
	case scratchKind:
		return "Temporary location. It may contain active work, exports, or diagnostics."
	case checkoutKind:
		return "Project checkout or worktree. Check for uncommitted work before cleanup."
	case buildKind:
		return "Generated output. Check whether a release, build, or test still needs it."
	case recoveryKind:
		return "Previous Windows installation. Review it in Windows Storage settings."
	case dockerKind:
		return "Docker Desktop virtual disk. The Docker tab shows the resources inside it."
	case appKind:
		return "Application data may include sessions, settings, or virtual machine state."
	case systemKind:
		return "Windows and installed application files. Review them in Windows Storage settings."
	default:
		return "Review the location in Explorer or inspect the folder."
	}
}

func quickTargets(ctx context.Context, reader *directoryReader, roots []string, meta *coverage) []target {
	home, _ := os.UserHomeDir()
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")
	var paths []target
	add := func(path, kind string, split bool) {
		if path != "" {
			paths = append(paths, target{path, kind, split})
		}
	}
	for _, pair := range [][3]string{{"CODEX_HOME", ".codex", appKind}, {"CLAUDE_CONFIG_DIR", ".claude", appKind}} {
		path := os.Getenv(pair[0])
		if path == "" && home != "" {
			path = filepath.Join(home, pair[1])
		}
		add(path, pair[2], true)
	}
	if home != "" {
		add(filepath.Join(home, "Documents", "Codex"), projectKind, true)
	}
	if local != "" {
		add(filepath.Join(local, "Stugu", "tooling"), projectKind, true)
		add(filepath.Join(local, "OpenAI", "Codex"), appKind, true)
		for _, name := range []string{"Claude-Data", "AnthropicClaude", "Codex", "Claude", "Temp"} {
			kind := appKind
			if name == "Temp" {
				kind = scratchKind
			}
			add(filepath.Join(local, name), kind, true)
		}
		packages := filepath.Join(local, "Packages")
		if _, err := localFolder(packages); err == nil {
			err := reader.entries(ctx, packages, 0, func(entry dirEntry) {
				lower := strings.ToLower(entry.Name)
				if entry.directory() && !entry.link() && (strings.HasPrefix(lower, "openai.codex") ||
					strings.HasPrefix(lower, "claude_") || strings.HasPrefix(lower, "anthropic")) {
					add(filepath.Join(packages, entry.Name), appKind, true)
				}
			})
			reader.addGap(meta, packages, err)
		} else {
			reader.addGap(meta, packages, err)
		}
	}
	if roaming != "" {
		for _, app := range []string{"Codex", "Claude"} {
			add(filepath.Join(roaming, app), appKind, true)
		}
	}
	for _, root := range roots {
		add(filepath.Join(root, "tmp"), scratchKind, true)
		add(filepath.Join(root, "temp"), scratchKind, true)
	}
	for _, disk := range dockerDisks() {
		add(disk, dockerKind, false)
	}
	var findBackups func(string, bool)
	findBackups = func(root string, includeChildren bool) {
		if ctx.Err() != nil {
			return
		}
		if _, err := localFolder(root); err != nil {
			reader.addGap(meta, root, err)
			return
		}
		var children []string
		err := reader.entries(ctx, root, 0, func(entry dirEntry) {
			if entry.link() {
				return
			}
			path := filepath.Join(root, entry.Name)
			kind := classify(entry.Name, filesKind, entry.directory(), root == volumeRoot(root))
			if kind == backupKind || kind == recoveryKind {
				add(path, kind, false)
			} else if includeChildren && entry.directory() && !strings.HasPrefix(entry.Name, ".") {
				children = append(children, path)
			}
		})
		reader.addGap(meta, root, err)
		for _, child := range children {
			findBackups(child, false)
		}
	}
	for _, root := range roots {
		findBackups(root, false)
	}
	if home != "" {
		findBackups(home, false)
		findBackups(filepath.Join(home, "Documents"), true)
		oneDrive := os.Getenv("OneDrive")
		if oneDrive == "" {
			oneDrive = filepath.Join(home, "OneDrive")
		}
		findBackups(filepath.Join(oneDrive, "Documents"), true)
	}
	var selected []target
	keys := make(map[string]bool)
	for _, t := range paths {
		path, err := filepath.Abs(t.Path)
		if err != nil || volumeRoot(path) == "" || !isFixed(volumeRoot(path)) {
			continue
		}
		folder := path
		if !t.Split {
			folder = filepath.Dir(path)
		}
		if _, err := localFolder(folder); err != nil {
			reader.addGap(meta, folder, err)
			continue
		}
		key := normalizedKey(path)
		if !keys[key] {
			keys[key] = true
			selected = append(selected, target{path, t.Kind, t.Split})
		}
	}
	slices.SortFunc(selected, func(a, b target) int {
		return strings.Compare(normalizedKey(a.Path), normalizedKey(b.Path))
	})
	return slices.DeleteFunc(selected, func(t target) bool {
		for parent := filepath.Dir(t.Path); parent != t.Path; parent = filepath.Dir(parent) {
			if keys[normalizedKey(parent)] {
				return true
			}
			if filepath.Dir(parent) == parent {
				break
			}
		}
		return false
	})
}

func (r *directoryReader) addGap(meta *coverage, path string, err error) {
	if err != nil && !os.IsNotExist(err) && !errors.Is(err, context.Canceled) {
		meta.Gaps = append(meta.Gaps, path)
		if r.gapKinds == nil {
			r.gapKinds = make(map[string]int)
		}
		r.gapKinds[diagnosticErrorKind(err)]++
	}
}

// scopeKey ties a folder's history to the volume it was on. Quick review spans
// every fixed drive, so its key leaves drives out: adding or removing a drive
// keeps the history, and calculateChanges compares only drives that match.
func scopeKey(scope string, disks []driveState) string {
	values := []string{"files-v1", normalizedKey(scope)}
	if scope != quickScope {
		for _, disk := range disks {
			if disk.Root == volumeRoot(scope) {
				values = append(values, disk.Root+"="+disk.ID)
			}
		}
	}
	return strings.Join(values, "|")
}

// savedScopeKey finds the history key for a location without scanning it.
func savedScopeKey(scope string) string {
	if root := volumeRoot(scope); scope != quickScope && root != "" {
		return scopeKey(scope, diskState([]string{root}))
	}
	return scopeKey(scope, nil)
}

func drivesReplaced(before, after []driveState) bool {
	for _, old := range before {
		for _, now := range after {
			if old.Root == now.Root && old.ID != now.ID {
				return true
			}
		}
	}
	return false
}

func captureFiles(ctx context.Context, scope, kind string, progress func(string, int64)) (_ snapshot, err error) {
	started := time.Now()
	var s snapshot
	reader := new(directoryReader)
	stage, scopeType := "validate_scope", captureScopeType(scope)
	var probeTime, discoverTime, walkTime, finalizeTime time.Duration
	var targetCount int
	defer func() {
		logDuration(ctx, "scan.files", started, err, slog.String("stage", stage), slog.String("scope_type", scopeType),
			slog.Int("targets", targetCount), slog.Int64("files_checked", s.Coverage.Checked),
			slog.Int64("directory_attempts", reader.directories), slog.Int64("metadata_batches", reader.batches),
			slog.Int64("fallback_directories", reader.estimated), slog.Int("unreadable", len(s.Coverage.Gaps)),
			slog.Int64("size_lookups", reader.sizeLookups), slog.Int64("size_lookup_failures", reader.sizeFailures),
			slog.Any("gap_kinds", reader.gapKinds), slog.Int64("repeated_links", s.Coverage.Shared),
			slog.Int64("skipped_links", s.Coverage.Links), slog.Int64("online_only", s.Coverage.Online),
			slog.Int("readings", len(s.Records)), slog.Float64("drive_probe_ms", milliseconds(probeTime)),
			slog.Float64("discover_ms", milliseconds(discoverTime)), slog.Float64("walk_ms", milliseconds(walkTime)),
			slog.Float64("finalize_ms", milliseconds(finalizeTime)))
	}()
	if scope != quickScope {
		path, err := localFolder(scope)
		if err != nil {
			return s, err
		}
		scope = path
		if kind == "" {
			kind = classify(filepath.Base(path), filesKind, true, filepath.Dir(path) == volumeRoot(path))
		}
	}
	if kind == "" {
		kind = filesKind
	}
	stage = "drive_probe"
	phaseStarted := time.Now()
	roots := driveRoots()
	if scope != quickScope {
		roots = []string{volumeRoot(scope)}
	}
	startDisks := diskState(roots)
	probeTime = time.Since(phaseStarted)
	if len(startDisks) == 0 {
		return s, errors.New("no local fixed drive was available")
	}
	s.Scope, s.Label = scopeKey(scope, startDisks), scope
	s.Coverage = coverage{ScopeKind: "files", Kind: kind, Subfolders: true}
	stage = "discover"
	phaseStarted = time.Now()
	targets := []target{{scope, kind, true}}
	if scope == quickScope {
		targets = quickTargets(ctx, reader, roots, &s.Coverage)
	}
	targetCount = len(targets)
	discoverTime = time.Since(phaseStarted)
	stage = "walk"
	phaseStarted = time.Now()
	sized := make(map[fileID]struct{})
	var drive string
	lastProgress := time.Time{}
	mark := func(path string) {
		if progress != nil && time.Since(lastProgress) >= 200*time.Millisecond {
			progress(path, s.Coverage.Checked)
			lastProgress = time.Now()
		}
	}
	measure := func(entry dirEntry, parent, kind string, bucket, review, childFolder *record) {
		s.Coverage.Checked++
		size := entry.Bytes
		if size == 0 && entry.Length > 0 && entry.Attrs&attrsOffline != 0 {
			s.Coverage.Online++
		}
		if size > 0 && entry.ID != (fileID{}) {
			if _, seen := sized[entry.ID]; seen {
				s.Coverage.Shared++
				size = 0
			} else {
				sized[entry.ID] = struct{}{}
			}
		}
		addAllocation(bucket, size, entry.Modified)
		if review != nil && review != bucket {
			addAllocation(review, size, entry.Modified)
		}
		if childFolder != nil {
			addAllocation(childFolder, size, entry.Modified)
		}
		if size >= fileFloor {
			item := *bucket
			if bucket.Directory {
				item = pathRecord(filepath.Join(parent, entry.Name), classify(entry.Name, kind, false, false), entry)
				item.Bytes, item.Files = size, 1
			}
			item.Series = largeFiles
			s.Records = append(s.Records, item)
			if review == nil && isCandidate(item.Kind) {
				item.Series = candidates
				s.Records = append(s.Records, item)
			}
		}
	}
	type branch struct {
		path, kind string
		review     *record
		child      *record
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		if newDrive := volumeRoot(t.Path); newDrive != drive {
			sized = make(map[fileID]struct{})
			drive = newDrive
		}
		parent := t.Path
		if !t.Split {
			parent = filepath.Dir(t.Path)
		}
		var entries []dirEntry
		err := reader.entries(ctx, parent, 0, func(entry dirEntry) {
			if t.Split || strings.EqualFold(entry.Name, filepath.Base(t.Path)) {
				entries = append(entries, entry)
			}
		})
		reader.addGap(&s.Coverage, parent, err)
		slices.SortFunc(entries, func(a, b dirEntry) int { return strings.Compare(a.Name, b.Name) })
		for _, entry := range entries {
			if ctx.Err() != nil {
				break
			}
			if entry.link() {
				s.Coverage.Links++
				continue
			}
			path := filepath.Join(parent, entry.Name)
			category := t.Kind
			if t.Split {
				category = classify(entry.Name, t.Kind, entry.directory(), parent == volumeRoot(parent))
			}
			bucket := pathRecord(path, category, entry)
			var firstReview *record
			if isCandidate(category) {
				firstReview = &bucket
			}
			var reviewItems []*record
			var childItems []*record
			if entry.directory() {
				pending := []branch{{path: path, kind: category, review: firstReview}}
				for len(pending) != 0 && ctx.Err() == nil {
					current := pending[len(pending)-1]
					pending = pending[:len(pending)-1]
					mark(current.path)
					err := reader.entries(ctx, current.path, fileFloor, func(child dirEntry) {
						if child.link() {
							s.Coverage.Links++
						} else if child.directory() {
							nextPath := filepath.Join(current.path, child.Name)
							nextKind := classify(child.Name, current.kind, true, false)
							childFolder := current.child
							if current.path == bucket.Path {
								item := pathRecord(nextPath, nextKind, child)
								item.Series = subfolders
								childFolder = &item
								childItems = append(childItems, childFolder)
							}
							review := current.review
							if review == nil && isCandidate(nextKind) {
								item := pathRecord(nextPath, nextKind, child)
								review = &item
								reviewItems = append(reviewItems, review)
							}
							pending = append(pending, branch{nextPath, nextKind, review, childFolder})
						} else {
							measure(child, current.path, current.kind, &bucket, current.review, current.child)
						}
					})
					reader.addGap(&s.Coverage, current.path, err)
				}
			} else {
				measure(entry, parent, category, &bucket, firstReview, nil)
			}
			if bucket.Bytes > 0 {
				s.Records = append(s.Records, bucket)
			}
			for _, item := range childItems {
				if item.Bytes >= fileFloor {
					s.Records = append(s.Records, *item)
				}
			}
			if firstReview != nil {
				reviewItems = append(reviewItems, firstReview)
			}
			for _, item := range reviewItems {
				if item.Bytes >= mib {
					copy := *item
					copy.Series = candidates
					s.Records = append(s.Records, copy)
				}
			}
		}
	}
	walkTime = time.Since(phaseStarted)
	if err := ctx.Err(); err != nil {
		return snapshot{}, err
	}
	s.Coverage.Estimated = reader.estimated
	stage = "finalize"
	phaseStarted = time.Now()
	s.Coverage.Drives = diskState(roots)
	finalizeTime = time.Since(phaseStarted)
	if scopeKey(scope, s.Coverage.Drives) != s.Scope || drivesReplaced(startDisks, s.Coverage.Drives) {
		return snapshot{}, errors.New("a drive changed during the scan; scan again for a comparable snapshot")
	}
	slices.Sort(s.Coverage.Gaps)
	s.Coverage.Gaps = slices.Compact(s.Coverage.Gaps)
	s.Captured = time.Now().UnixMilli()
	return s, nil
}

func addAllocation(item *record, size, modified int64) {
	item.Bytes += size
	item.Files++
	item.Modified = max(item.Modified, modified)
}

func pathRecord(path, kind string, entry dirEntry) record {
	key := normalizedKey(path)
	if entry.ID != (fileID{}) {
		key = fmt.Sprintf("%s%016x%016x", volumeRoot(path), entry.ID.High, entry.ID.Low)
	}
	name := path
	if parent := filepath.Dir(path); parent != volumeRoot(parent) {
		name = filepath.Base(parent) + " / " + filepath.Base(path)
	}
	return record{Series: folders, Key: key, Name: name, Path: path, Kind: kind,
		Directory: entry.directory(), Modified: entry.Modified}
}

func refreshAtStartup(saved snapshot, current []driveState) bool {
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
