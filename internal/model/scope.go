package model

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
)

// VolumeRoot returns "C:\" for a path on drive C, or "" for other paths.
func VolumeRoot(path string) string {
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return ""
	}
	return strings.ToUpper(volume) + `\`
}

// NormalizedKey compares Windows paths without case or trailing separators.
func NormalizedKey(path string) string {
	return strings.TrimRight(strings.ToLower(filepath.Clean(path)), `\`)
}

// ScopeKey ties a folder's history to the volume that holds it. Quick review
// spans every fixed drive, so its key leaves drives out: adding or removing a
// drive keeps its history, and comparisons count only drives that match.
func ScopeKey(scope string, disks []DriveState) string {
	values := []string{"files-v1", NormalizedKey(scope)}
	if scope != QuickScope {
		for _, disk := range disks {
			if disk.Root == VolumeRoot(scope) {
				values = append(values, disk.Root+"="+disk.ID)
			}
		}
	}
	return strings.Join(values, "|")
}

// ScopeType names the kind of location for diagnostics: docker, quick, drive or folder.
func ScopeType(scope string) string {
	root := VolumeRoot(scope)
	switch {
	case strings.EqualFold(scope, DockerScope):
		return "docker"
	case scope == "" || strings.EqualFold(scope, QuickScope):
		return "quick"
	case root != "" && strings.EqualFold(strings.TrimRight(scope, `\/`), strings.TrimRight(root, `\`)):
		return "drive"
	default:
		return "folder"
	}
}

// SupportedSeries lists the series of a scope kind. Comparisons of Docker
// captures lead with the engine totals. Category totals are not compared:
// they span drives and unreadable locations that can differ between captures.
func SupportedSeries(kind string, comparing bool) []string {
	if kind == ScopeDocker {
		if comparing {
			return []string{DockerTotals, Projects, Volumes, Containers, Images, BuildCache}
		}
		return []string{Projects, Volumes, Containers, Images, BuildCache, DockerTotals}
	}
	if comparing {
		return []string{Folders, Subfolders, Candidates, Labeled, LargeFiles}
	}
	return []string{Folders, Subfolders, Categories, Candidates, Labeled, LargeFiles}
}

// ChooseSeries returns the canonical series name for a request, or the first
// series of the kind when wanted is empty.
func ChooseSeries(kind, wanted string, comparing bool) (string, error) {
	valid := SupportedSeries(kind, comparing)
	if wanted == "" {
		return valid[0], nil
	}
	for _, series := range valid {
		if strings.EqualFold(series, wanted) {
			return series, nil
		}
	}
	return "", InvalidArgument("choose one of %s", strings.Join(valid, ", "))
}

// SortRecords orders records by size, largest first, then by name.
func SortRecords(items []Record) {
	slices.SortStableFunc(items, func(a, b Record) int {
		if order := cmp.Compare(b.Bytes, a.Bytes); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// RecordsIn returns one series of a snapshot, largest first.
func RecordsIn(s Snapshot, series string) []Record {
	var items []Record
	for _, item := range s.Records {
		if item.Series == series {
			items = append(items, item)
		}
	}
	SortRecords(items)
	return items
}

// DriveChanges pairs drives that hold the same volume in both readings.
func DriveChanges(before, after []DriveState) []DriveDelta {
	var result []DriveDelta
	for _, now := range after {
		for _, old := range before {
			if now.Root == old.Root && now.ID == old.ID && now.Total == old.Total {
				result = append(result, DriveDelta{Root: now.Root, UsedDelta: old.Free - now.Free, Free: now.Free})
			}
		}
	}
	return result
}
