package model

// Categories assigned to scanned locations.
const (
	FilesKind    = "Files"
	ProjectKind  = "Project files"
	AppKind      = "App data"
	ScratchKind  = "Scratch"
	BackupKind   = "Backups"
	CacheKind    = "Cache & logs"
	CheckoutKind = "Checkouts"
	BuildKind    = "Build output"
	RecoveryKind = "Windows recovery"
	DockerKind   = "Docker storage"
	SystemKind   = "System files"
)

// IsCandidate reports whether a category is worth reviewing for cleanup.
func IsCandidate(kind string) bool {
	switch kind {
	case BackupKind, CacheKind, ScratchKind, CheckoutKind, BuildKind, RecoveryKind:
		return true
	}
	return false
}

// KindNote explains what to check before cleaning up a category.
func KindNote(kind string) string {
	switch kind {
	case BackupKind:
		return "Backup or recovery data. Check which copies you still need."
	case CacheKind:
		return "Cache or diagnostic files. Check whether the owning app is using them."
	case ScratchKind:
		return "Temporary location. It may contain active work, exports, or diagnostics."
	case CheckoutKind:
		return "Project checkout or worktree. Check for uncommitted work before cleanup."
	case BuildKind:
		return "Generated output. Check whether a release, build, or test still needs it."
	case RecoveryKind:
		return "Previous Windows installation. Review it in Windows Storage settings."
	case DockerKind:
		return "Docker Desktop virtual disk. The Docker tab shows the resources inside it."
	case AppKind:
		return "Application data may include sessions, settings, or virtual machine state."
	case SystemKind:
		return "Windows and installed application files. Review them in Windows Storage settings."
	default:
		return "Review the location in Explorer or inspect the folder."
	}
}
