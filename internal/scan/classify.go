package scan

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/liljestk/VIbeClean/internal/model"
)

var (
	backupName = regexp.MustCompile(`backup|filehistory|windowsimagebackup|(?:^|[-_. ])(?:recover(?:y|ed|_all|\d+)?|restored?|rollback)(?:$|[-_. ])`)
	backupExt  = regexp.MustCompile(`\.(?:bak\d*|bkp|backup|hsvbak|dump)(?:$|\.(?:zip|gz|zst)$)`)
	archiveExt = regexp.MustCompile(`\.(?:zip|7z|tar|gz|zst|rar|img|vhdx?)$`)
	virtualExt = regexp.MustCompile(`\.(?:vhdx?|avhdx|vmdk|vdi|qcow2)$`)
)

// place is the folder that holds an entry: its category and its path in
// lower case without a trailing separator, such as `c:\users\ana`.
type place struct {
	kind  string
	lower string
}

func placeOf(path, kind string) place {
	return place{kind: kind, lower: model.NormalizedKey(path)}
}

// child returns the place of a subfolder with the given label.
func (p place) child(name, kind string) place {
	return place{kind: kind, lower: p.lower + `\` + strings.ToLower(name)}
}

// root reports whether the place is a drive root.
func (p place) root() bool { return len(p.lower) == 2 }

// name returns the folder's own name in lower case.
func (p place) name() string { return p.lower[strings.LastIndexByte(p.lower, '\\')+1:] }

// label is the category of an entry. A unit is the top folder of an
// installed program or game, or of a project, and is listed on its own. why
// explains labels that come from where a launcher or tool puts its files.
type label struct {
	kind string
	unit bool
	why  string
}

// classify labels a folder entry. Folders inherit the category of their
// parent; Windows, installed software, caches, generated output and backups
// keep theirs for everything inside them, apart from the cache and download
// folders game launchers keep beside the games.
func classify(name string, parent place, directory bool) label {
	lower := strings.ToLower(name)
	if !directory {
		return label{kind: fileKind(lower, parent)}
	}
	if l, ok := libraryLabel(lower, parent); ok {
		return l
	}
	if sticky(parent.kind) {
		return label{kind: parent.kind}
	}
	if parent.root() {
		if kind, ok := rootKind(lower); ok {
			return label{kind: kind}
		}
	}
	if l, ok := profileLabel(lower, parent); ok {
		return l
	}
	return label{kind: folderKind(lower, parent)}
}

// kindOfPath labels a folder by walking its path from the drive root.
func kindOfPath(path string) string {
	root := model.VolumeRoot(path)
	at := placeOf(root, model.FilesKind)
	rest := filepath.Clean(path)
	if len(rest) <= len(root) {
		return at.kind
	}
	for _, part := range strings.Split(rest[len(root):], `\`) {
		if part != "" {
			at = at.child(part, classify(part, at, true).kind)
		}
	}
	return at.kind
}

// sticky reports whether everything inside a category keeps it.
func sticky(kind string) bool {
	switch kind {
	case model.SystemKind, model.RecoveryKind, model.DockerKind, model.BackupKind, model.RecycleKind,
		model.VirtualKind, model.TempKind, model.PackageKind, model.CacheKind, model.BuildKind,
		model.GameKind, model.ProgramKind:
		return true
	}
	return false
}

func rootKind(lower string) (string, bool) {
	switch lower {
	case "windows.old", "$windows.~bt", "$windows.~ws", "$winreagent", "$sysreset":
		return model.RecoveryKind, true
	case "$recycle.bin":
		return model.RecycleKind, true
	case "program files", "program files (x86)":
		return model.ProgramKind, true
	case "xboxgames", "steamlibrary":
		return model.GameKind, true
	case "programdata", "recovery", "system volume information", "windows", "windowsapps", "wpsystem":
		return model.SystemKind, true
	case "perflogs":
		return model.PossibleCleanupKind, true
	}
	return "", false
}

// libraryLabel finds installed programs and games by where installers and
// game launchers put them. It applies inside program and game folders too,
// because launchers keep their libraries there.
func libraryLabel(lower string, parent place) (label, bool) {
	dir, base := parent.lower, parent.name()
	switch parent.kind {
	case model.FilesKind, model.PersonalKind, model.ProgramKind, model.GameKind:
	case model.AppKind:
		// Launchers keep settings and overlays in app data, not games; only
		// per-user program installs live there.
		if lower == "programs" && strings.HasSuffix(dir, `\appdata\local`) {
			return label{kind: model.ProgramKind}, true
		}
		return label{}, false
	default:
		return label{}, false
	}
	game := func(why string) (label, bool) { return label{kind: model.GameKind, unit: true, why: why}, true }
	switch {
	case lower == "steamapps" || lower == "steamlibrary":
		return label{kind: model.GameKind}, true
	case base == "steamapps":
		switch lower {
		case "shadercache":
			if programsFolder(filepath.Dir(filepath.Dir(dir))) && filepath.Base(filepath.Dir(dir)) == "steam" {
				return label{kind: model.CacheKind, why: "Steam keeps compiled shaders here and rebuilds them."}, true
			}
			return label{kind: model.PossibleCleanupKind, why: "The path resembles Steam's shader cache."}, true
		case "downloading", "temp":
			return label{kind: model.PossibleCleanupKind, why: "The path resembles a Steam staging folder."}, true
		case "workshop":
			return game("Steam Workshop content for installed games.")
		}
		return label{kind: model.GameKind}, true
	case base == "common" && strings.HasSuffix(dir, `\steamapps\common`):
		return game("Installed with Steam.")
	case base == "steam" && parent.kind == model.ProgramKind && programsFolder(filepath.Dir(dir)):
		switch lower {
		case "appcache", "htmlcache", "logs", "dumps":
			return label{kind: model.CacheKind, why: "Steam client cache or logs."}, true
		}
	case base == "epic games":
		switch lower {
		case "launcher", "epic online services", "directxredist":
			return label{kind: model.ProgramKind, unit: true, why: "Part of the Epic Games Launcher."}, true
		}
		return game("Installed with the Epic Games Launcher.")
	case base == "games" && strings.HasSuffix(dir, `\gog galaxy\games`), base == "gog games":
		return game("Installed with GOG Galaxy.")
	case base == "games" && strings.HasSuffix(dir, `\ubisoft game launcher\games`):
		return game("Installed with Ubisoft Connect.")
	case base == "xboxgames", base == "modifiablewindowsapps":
		return game("Installed with the Xbox app.")
	case base == "ea games", base == "origin games":
		return game("Installed with the EA app.")
	case base == "riot games":
		if lower == "riot client" {
			return label{kind: model.ProgramKind, unit: true, why: "Riot Client launcher."}, true
		}
		return game("Installed with the Riot Client.")
	}
	switch lower {
	case "epic games", "gog games", "xboxgames", "modifiablewindowsapps", "ea games", "origin games", "riot games":
		return label{kind: model.GameKind}, true
	}
	if programsFolder(dir) {
		switch {
		case lower == "windowsapps" || lower == "internet explorer" || lower == "windowspowershell" ||
			strings.HasPrefix(lower, "windows "):
			return label{kind: model.SystemKind}, true
		case lower == "common files" || lower == "common":
			// Shared components belong to several programs, so there is no one program to uninstall.
			return label{kind: model.ProgramKind}, true
		}
		return label{kind: model.ProgramKind, unit: true, why: "Installed program."}, true
	}
	return label{}, false
}

// programsFolder reports whether a folder holds one folder per installed program.
func programsFolder(dir string) bool {
	if len(dir) > 2 && dir[1] == ':' {
		if rest := dir[2:]; rest == `\program files` || rest == `\program files (x86)` {
			return true
		}
	}
	return strings.HasSuffix(dir, `\appdata\local\programs`)
}

// profileLabel labels the standard folders of a user profile. Hidden
// folders in a profile hold tool settings and data.
func profileLabel(lower string, parent place) (label, bool) {
	dir := parent.lower
	if !profileFolder(dir) {
		return label{}, false
	}
	switch lower {
	case "appdata":
		return label{kind: model.AppKind}, true
	case "downloads":
		return label{kind: model.DownloadKind}, true
	case "documents", "desktop", "pictures", "videos", "music", "saved games", "favorites", "contacts",
		"dropbox", "google drive", "icloud drive", "iclouddrive":
		return label{kind: model.PersonalKind}, true
	case ".cache", ".npm":
		return label{}, false
	}
	switch {
	case strings.HasPrefix(lower, "onedrive"):
		return label{kind: model.PersonalKind}, true
	case strings.HasPrefix(lower, "."):
		return label{kind: model.AppKind}, true
	}
	return label{}, false
}

func profileFolder(dir string) bool {
	return len(dir) >= 10 && dir[1] == ':' && strings.HasPrefix(dir[2:], `\users\`) && strings.Count(dir, `\`) == 2
}

func fileKind(lower string, parent place) string {
	if sticky(parent.kind) {
		return parent.kind
	}
	if parent.root() {
		switch lower {
		case "pagefile.sys", "hiberfil.sys", "swapfile.sys", "dumpstack.log", "dumpstack.log.tmp":
			return model.SystemKind
		}
	}
	switch lower {
	case "docker_data.vhdx", "dockerdesktop.vhdx", "dockerdesktopvm.vhdx":
		return model.DockerKind
	}
	if backupExt.MatchString(lower) || (archiveExt.MatchString(lower) && backupName.MatchString(lower)) {
		return model.BackupKind
	}
	if virtualExt.MatchString(lower) {
		return model.VirtualKind
	}
	for _, suffix := range []string{".log", ".log.gz", ".log.1", ".dmp"} {
		if strings.HasSuffix(lower, suffix) {
			return model.PossibleCleanupKind
		}
	}
	return parent.kind
}

func folderKind(lower string, parent place) string {
	if backupName.MatchString(lower) {
		return model.BackupKind
	}
	dir, base := parent.lower, parent.name()
	switch {
	case lower == "temp" && profileLocation(dir, `\appdata\local`):
		return model.TempKind
	case packageCache(lower, dir, base):
		if managedPackageCache(lower, dir) {
			return model.PackageKind
		}
		return model.PossibleCleanupKind
	case lower == "avd" && base == ".android":
		return model.VirtualKind
	}
	switch lower {
	case "worktrees", "release-checkouts", "source-checkout", "checkouts":
		return model.CheckoutKind
	case "cache", "caches", ".cache", "gpucache", "code cache", "localcache", "paste-cache",
		"shadercache", "dawncache", "logs", "log", "debug", "crashdumps", "inetcache", "cachestorage",
		"dxcache", "glcache", "d3dscache", "nv_cache", "grshadercache", "graphitedawncache", "crashpad",
		"go-build":
		return model.PossibleCleanupKind
	case "temp", "tmp", ".tmp":
		return model.ScratchKind
	case "virtualbox vms", "virtual machines", "virtual hard disks", ".vagrant.d":
		return model.VirtualKind
	case "node_modules", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".next",
		".nuxt", ".turbo", ".parcel-cache", ".svelte-kit", ".angular":
		switch parent.kind {
		case model.FilesKind, model.PersonalKind, model.DownloadKind, model.ProjectKind, model.CheckoutKind, model.ScratchKind:
			return model.BuildKind
		}
	case "dist", "target", "coverage", "playwright-report", "test-results", ".venv", "venv":
		switch parent.kind {
		case model.FilesKind, model.PersonalKind, model.ProjectKind, model.CheckoutKind, model.ScratchKind:
			return model.BuildKind
		}
	}
	if strings.HasSuffix(lower, "-wt") || strings.HasSuffix(lower, "-worktree") {
		return model.CheckoutKind
	}
	return parent.kind
}

// managedPackageCache recognizes default tool locations under a Windows profile.
func managedPackageCache(lower, dir string) bool {
	switch lower {
	case ".npm":
		return profileFolder(dir)
	case "npm-cache", "ms-playwright":
		return profileLocation(dir, `\appdata\local`)
	case "packages":
		return profileLocation(dir, `\.nuget`)
	case "repository":
		return profileLocation(dir, `\.m2`)
	case "registry", "git":
		return profileLocation(dir, `\.cargo`)
	case "mod":
		return profileLocation(dir, `\go\pkg`)
	}
	return false
}

func profileLocation(dir, suffix string) bool {
	return strings.HasSuffix(dir, suffix) && profileFolder(strings.TrimSuffix(dir, suffix))
}

// packageCache reports folders where package managers keep downloaded packages.
func packageCache(lower, dir, base string) bool {
	switch lower {
	case "npm-cache", ".npm", "pnpm-store", "v3-cache", "ms-playwright":
		return true
	case "packages":
		return base == ".nuget"
	case "repository":
		return base == ".m2"
	case "registry", "git":
		return base == ".cargo"
	case "store":
		return base == "pnpm"
	case "wrapper":
		return base == ".gradle"
	case "mod":
		return strings.HasSuffix(dir, `\go\pkg`)
	case "pkgs":
		return base == "anaconda3" || base == "miniconda3" || base == "miniforge3" || base == ".conda"
	}
	return false
}

// contentLabel labels a folder by what it holds: a CACHEDIR.TAG file marks a
// cache, version control or a project manifest marks a project, and launcher
// manifests or a game engine runtime mark an installed game. Only folders
// without a more specific label are relabeled.
func contentLabel(path string, entries []dirEntry, kind string) (label, bool) {
	var cache, project, game bool
	switch kind {
	case model.FilesKind, model.PersonalKind, model.PossibleCleanupKind:
		cache, project, game = true, true, true
	case model.ProjectKind:
		cache, project = true, true
	case model.AppKind:
		cache = true
	case model.BuildKind, model.CheckoutKind, model.DownloadKind, model.ScratchKind:
		cache = true
	case model.ProgramKind:
		game = true
	default:
		return label{}, false
	}
	var found label
	for _, entry := range entries {
		l, ok := marker(entry, project, game)
		if cache && validCacheTag(path, entry) {
			l, ok = label{kind: model.CacheKind, why: "Contains a valid CACHEDIR.TAG signature."}, true
		}
		if !ok {
			continue
		}
		// Keep a project or game when it also contains a cache tag.
		if found.kind == "" || l.kind == model.ProjectKind || found.kind == model.CacheKind {
			found = l
		}
	}
	return found, found.kind != ""
}

// markerFile reports whether a lower-case file name can identify its folder.
// Directory listings decode these names even for small files.
func markerFile(lower string) bool {
	switch lower {
	case "cachedir.tag", ".git", "go.mod", "cargo.toml", "package.json", "pyproject.toml", "pom.xml",
		"build.gradle", "build.gradle.kts", ".build.info", "unityplayer.dll", "steam_appid.txt":
		return true
	}
	return strings.HasSuffix(lower, ".sln") || (strings.HasPrefix(lower, "goggame-") && strings.HasSuffix(lower, ".info"))
}

func marker(entry dirEntry, project, game bool) (label, bool) {
	name := entry.Name
	if name == "" || len(name) > 32 || entry.link() {
		return label{}, false
	}
	lower := strings.ToLower(name)
	dir := entry.directory()
	switch {
	case project && (lower == ".git" || (dir && (lower == ".hg" || lower == ".svn"))):
		return label{kind: model.ProjectKind, unit: true, why: "Contains a version control repository."}, true
	case project && !dir && (lower == "go.mod" || lower == "cargo.toml" || lower == "package.json" ||
		lower == "pyproject.toml" || lower == "pom.xml" || lower == "build.gradle" || lower == "build.gradle.kts" ||
		strings.HasSuffix(lower, ".sln")):
		return label{kind: model.ProjectKind, unit: true, why: "Contains a project file (" + name + ")."}, true
	case game && dir && lower == ".egstore":
		return label{kind: model.GameKind, unit: true, why: "Installed with the Epic Games Launcher."}, true
	case game && !dir && lower == ".build.info":
		return label{kind: model.GameKind, unit: true, why: "Installed with Battle.net."}, true
	case game && !dir && strings.HasPrefix(lower, "goggame-") && strings.HasSuffix(lower, ".info"):
		return label{kind: model.GameKind, unit: true, why: "Installed with GOG Galaxy."}, true
	case game && !dir && (lower == "unityplayer.dll" || lower == "steam_appid.txt"):
		return label{kind: model.GameKind, unit: true, why: "Contains a game runtime (" + name + ")."}, true
	}
	return label{}, false
}
