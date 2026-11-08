package scan

import (
	"regexp"
	"strings"

	"github.com/liljestk/VIbeClean/internal/model"
)

var (
	backupName = regexp.MustCompile(`backup|filehistory|windowsimagebackup|(?:^|[-_. ])(?:recover(?:y|ed|_all|\d+)?|restored?|rollback)(?:$|[-_. ])`)
	backupExt  = regexp.MustCompile(`\.(?:bak\d*|bkp|backup|hsvbak|dump)(?:$|\.(?:zip|gz|zst)$)`)
	archiveExt = regexp.MustCompile(`\.(?:zip|7z|tar|gz|zst|rar|img|vhdx?)$`)
)

// classify assigns a category to a folder entry. Categories inherit from the
// parent, and system, recovery, Docker and backup folders keep theirs for
// everything inside them.
func classify(name, parent string, directory, atDriveRoot bool) string {
	switch parent {
	case model.SystemKind, model.RecoveryKind, model.DockerKind, model.BackupKind:
		return parent
	}
	lower := strings.ToLower(name)
	if atDriveRoot && directory {
		if kind, ok := rootKind(lower); ok {
			return kind
		}
	}
	if !directory {
		return fileKind(lower, parent)
	}
	return folderKind(lower, parent)
}

func rootKind(lower string) (string, bool) {
	switch lower {
	case "windows.old":
		return model.RecoveryKind, true
	case "$recycle.bin", "program files", "program files (x86)", "programdata", "recovery",
		"system volume information", "windows", "windowsapps", "wpsystem":
		return model.SystemKind, true
	}
	return "", false
}

func fileKind(lower, parent string) string {
	switch lower {
	case "docker_data.vhdx", "dockerdesktop.vhdx", "dockerdesktopvm.vhdx":
		return model.DockerKind
	}
	if backupExt.MatchString(lower) || (archiveExt.MatchString(lower) && backupName.MatchString(lower)) {
		return model.BackupKind
	}
	for _, suffix := range []string{".log", ".log.gz", ".log.1", ".dmp"} {
		if strings.HasSuffix(lower, suffix) {
			return model.CacheKind
		}
	}
	return parent
}

func folderKind(lower, parent string) string {
	if backupName.MatchString(lower) {
		return model.BackupKind
	}
	switch lower {
	case "worktrees", "release-checkouts", "source-checkout", "checkouts":
		return model.CheckoutKind
	case "cache", "caches", ".cache", "gpucache", "code cache", "localcache", "paste-cache",
		"shadercache", "dawncache", "logs", "log", "debug", "crashdumps":
		return model.CacheKind
	case "temp", "tmp", ".tmp":
		return model.ScratchKind
	case "node_modules", "dist", "target", "coverage", "playwright-report", "test-results":
		if parent == model.FilesKind || parent == model.ProjectKind || parent == model.CheckoutKind || parent == model.ScratchKind {
			return model.BuildKind
		}
	}
	if strings.HasSuffix(lower, "-wt") || strings.HasSuffix(lower, "-worktree") {
		return model.CheckoutKind
	}
	return parent
}
