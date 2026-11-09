package model

// Categories assigned to scanned locations. They are stored in the index and
// must not change.
const (
	FilesKind    = "Files"
	PersonalKind = "Personal files"
	ProjectKind  = "Project files"
	AppKind      = "App data"
	ProgramKind  = "Programs"
	GameKind     = "Games"
	DownloadKind = "Downloads"
	ScratchKind  = "Scratch"
	TempKind     = "Temporary files"
	BackupKind   = "Backups"
	CacheKind    = "Cache & logs"
	PackageKind  = "Package caches"
	CheckoutKind = "Checkouts"
	BuildKind    = "Build output"
	RecycleKind  = "Recycle Bin"
	VirtualKind  = "Virtual disks"
	RecoveryKind = "Windows recovery"
	DockerKind   = "Docker storage"
	SystemKind   = "System files"
)

// LabelsVersion identifies the labeling rules a capture used. Captures made
// with different rules can label the same folder differently, so review
// candidates and labeled folders are compared only within one version.
const LabelsVersion = 1

// Removal says how the space a category uses can be reclaimed.
type Removal string

// Removal levels, from the easiest win to the hardest.
const (
	// SafeToClear marks data that apps recreate or that was already deleted.
	SafeToClear Removal = "Safe to clear"
	// CheckFirst marks data that may hold something the user still needs.
	CheckFirst Removal = "Check first"
	// Uninstall marks installed software that should be removed by its installer or launcher.
	Uninstall Removal = "Uninstall"
	// Keep marks Windows, app state and the user's own files.
	Keep Removal = "Keep"
)

// Removals lists the removal levels from the easiest win to the hardest.
var Removals = []Removal{SafeToClear, CheckFirst, Uninstall, Keep}

// RemovalOf returns the removal level of a category.
func RemovalOf(kind string) Removal {
	switch kind {
	case CacheKind, BuildKind, PackageKind, TempKind, RecycleKind:
		return SafeToClear
	case ScratchKind, BackupKind, CheckoutKind, DownloadKind, VirtualKind, DockerKind, RecoveryKind:
		return CheckFirst
	case GameKind, ProgramKind:
		return Uninstall
	}
	return Keep
}

// RemovalNote explains a removal level.
func RemovalNote(r Removal) string {
	switch r {
	case SafeToClear:
		return "Safe to clear: apps and tools recreate this when they need it, or it was already deleted. Close the app that owns it first."
	case CheckFirst:
		return "Check first: this can hold something you still need. Look inside before you delete it."
	case Uninstall:
		return "Uninstall: remove it in Settings > Apps or in its launcher. Deleting the folder leaves settings and registrations behind."
	}
	return "Keep: Windows, app state or your own files. It is not a cleanup target."
}

// IsCandidate reports whether a category is worth reviewing for cleanup.
// Docker storage has its own view.
func IsCandidate(kind string) bool {
	removal := RemovalOf(kind)
	return (removal == SafeToClear || removal == CheckFirst) && kind != DockerKind
}

// KindNote explains what to check before cleaning up a category.
func KindNote(kind string) string {
	switch kind {
	case BackupKind:
		return "Backup or recovery data. Check which copies you still need."
	case CacheKind:
		return "Cache or diagnostic files. Check whether the owning app is using them."
	case PackageKind:
		return "Downloaded packages for developer tools. They download again on the next install or build."
	case TempKind:
		return "Windows temporary files. Files that apps still hold open stay behind. Storage Sense can clear these automatically."
	case ScratchKind:
		return "Temporary location. It may contain active work, exports, or diagnostics."
	case CheckoutKind:
		return "Project checkout or worktree. Check for uncommitted work before cleanup."
	case BuildKind:
		return "Generated output or installed dependencies. A build or install recreates it."
	case DownloadKind:
		return "Downloaded files and installers. Check for files you have not saved anywhere else."
	case RecycleKind:
		return "Deleted files waiting in the Recycle Bin. Empty it to free the space."
	case VirtualKind:
		return "Virtual machine, emulator or WSL disk. Remove the machine or distribution with its own tool."
	case RecoveryKind:
		return "Previous Windows installation. Review it in Windows Storage settings."
	case DockerKind:
		return "Docker Desktop virtual disk. The Docker tab shows the resources inside it."
	case GameKind:
		return "Installed game. Uninstall it in its launcher to free the space."
	case ProgramKind:
		return "Installed program. Uninstall it in Settings > Apps so Windows removes it cleanly."
	case ProjectKind:
		return "Project or source folder. Dependencies and build output inside it are labeled separately."
	case PersonalKind:
		return "Your documents, pictures, videos and other personal files."
	case AppKind:
		return "Application data may include sessions, settings, or virtual machine state."
	case SystemKind:
		return "Windows and installed application files. Review them in Windows Storage settings."
	default:
		return "Review the location in Explorer or inspect the folder."
	}
}

// RemovalTotals sums a capture's category totals by removal level. ok is
// false when the capture has no category totals, such as a capture made
// before categories were recorded.
func RemovalTotals(s Snapshot) (totals map[Removal]int64, ok bool) {
	totals = make(map[Removal]int64, len(Removals))
	for _, item := range s.Records {
		if item.Series == Categories && item.Bytes > 0 {
			totals[RemovalOf(item.Kind)] += item.Bytes
			ok = true
		}
	}
	return totals, ok
}
