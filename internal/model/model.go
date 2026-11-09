// Package model defines the readings, captures and comparisons shared by the
// scanners, the index, the service and every front end. It performs no I/O.
//
// Records and coverage are stored as JSON in the index, so their field names
// and JSON tags are part of the on-disk format.
package model

import "github.com/liljestk/VIbeClean/internal/units"

// Scope names and kinds.
const (
	QuickScope  = "Quick review"
	DockerScope = "Docker"
	ScopeFiles  = "files"
	ScopeDocker = "docker"
)

// Tracking thresholds.
const (
	// FileFloor is the size from which large files and subfolders are tracked.
	FileFloor = 64 * units.MiB
	// MinChange is the default smallest change worth reporting.
	MinChange = 256 * units.MiB
)

// Series names. They are stored in the index and must not change.
const (
	Folders    = "Folders"
	Subfolders = "Subfolders"
	Candidates = "Review candidates"
	LargeFiles = "Large files"
	// Categories holds one total per category; each file counts toward one.
	Categories = "Categories"
	// Labeled holds the top folder of each installed program, game and
	// project. A folder inside one of another category counts only toward
	// its own.
	Labeled      = "Apps, games & projects"
	DockerTotals = "Docker totals"
	Projects     = "Projects"
	Volumes      = "Volumes"
	Containers   = "Containers"
	Images       = "Images"
	BuildCache   = "Build cache"
)

// Record is one reading in a capture. Bytes is -1 when the size is unknown.
type Record struct {
	Series      string `json:"series"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Bytes       int64  `json:"bytes"`
	Files       int64  `json:"files,omitempty"`
	Modified    int64  `json:"modified_ms,omitempty"`
	Directory   bool   `json:"directory,omitempty"`
	Activity    string `json:"activity,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Group       string `json:"group,omitempty"`
	Idle        bool   `json:"idle,omitempty"`
	Reclaimable int64  `json:"reclaimable_bytes,omitempty"`
	Partial     bool   `json:"partial,omitempty"`
}

// DriveState is one reading of a local fixed drive.
type DriveState struct {
	Root    string `json:"root"`
	ID      string `json:"volume_id"`
	Total   int64  `json:"total_bytes"`
	Free    int64  `json:"free_bytes"`
	Journal uint64 `json:"journal_id,omitempty"`
	NextUSN int64  `json:"next_usn,omitempty"`
}

// Coverage describes what a capture measured and what it could not read.
type Coverage struct {
	Drives     []DriveState `json:"drives,omitempty"`
	Gaps       []string     `json:"gaps,omitempty"`
	Checked    int64        `json:"files_checked,omitempty"`
	Shared     int64        `json:"repeated_links,omitempty"`
	Online     int64        `json:"online_only,omitempty"`
	Links      int64        `json:"skipped_links,omitempty"`
	Estimated  int64        `json:"estimated_dirs,omitempty"`
	Subfolders bool         `json:"subfolders,omitempty"`
	Labels     int          `json:"labels,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Engine     string       `json:"engine,omitempty"`
	ScopeKind  string       `json:"scope_kind"`
}

// Snapshot is one saved capture. Captured is in Unix milliseconds.
type Snapshot struct {
	ID       int64    `json:"id"`
	Scope    string   `json:"scope_id"`
	Label    string   `json:"label"`
	Captured int64    `json:"captured_ms"`
	Coverage Coverage `json:"coverage"`
	Records  []Record `json:"records,omitempty"`
}

// Change is one difference between two captures. A bounded change is a lower
// limit because one side was below the tracking floor.
type Change struct {
	Series  string  `json:"series"`
	Before  *Record `json:"before,omitempty"`
	After   *Record `json:"after,omitempty"`
	Bytes   int64   `json:"delta_bytes"`
	Bounded bool    `json:"minimum_magnitude,omitempty"`
}

// Item returns the newest side of the change.
func (c Change) Item() Record {
	if c.After != nil {
		return *c.After
	}
	return *c.Before
}

// Comparison holds the two endpoints of a comparison and its ranked changes.
type Comparison struct {
	Latest      Snapshot `json:"latest"`
	Baseline    Snapshot `json:"baseline"`
	ShortWindow bool     `json:"short_window"`
	Omitted     int      `json:"omitted_coverage_changes"`
	Changes     []Change `json:"changes"`
}

// DriveDelta is the measured free-space movement of one drive.
type DriveDelta struct {
	Root      string `json:"drive"`
	UsedDelta int64  `json:"used_delta_bytes"`
	Free      int64  `json:"free_bytes"`
}

// RowKey identifies a record within a scope.
type RowKey struct {
	Series string
	Key    string
}
