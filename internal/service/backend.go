// Package service is the backend contract shared by every front end: the
// desktop UI, the MCP server and the gRPC API. Front ends depend only on the
// Backend interface, so they work the same against the in-process backend in
// service/local or a remote server reached through the gRPC client.
package service

import (
	"context"

	"github.com/liljestk/VIbeClean/internal/model"
)

// Backend reads history, compares captures and starts captures.
//
// Errors are classified with the model error kinds, so callers can test them
// with errors.Is(err, model.ErrNotFound) and similar, and context
// cancellation surfaces as context.Canceled, wherever the backend runs.
type Backend interface {
	// Info identifies the backend.
	Info(ctx context.Context) (Info, error)
	// Drives returns current readings of the local fixed drives.
	Drives(ctx context.Context) ([]model.DriveState, error)
	// Scopes returns the latest capture of every saved scope, newest first, without records.
	Scopes(ctx context.Context) ([]model.Snapshot, error)
	// History lists the saved captures of one scope in reverse capture order, without records.
	History(ctx context.Context, scope string) ([]model.Snapshot, error)
	// Inventory returns a page of the detailed entries of a current capture.
	Inventory(ctx context.Context, query InventoryQuery) (InventoryPage, error)
	// StreamInventory writes all matching rows in the same order as Inventory.
	StreamInventory(ctx context.Context, query InventoryQuery, emit func(model.Record) error) error
	// Duplicates verifies the content of same-size files in a current inventory.
	Duplicates(ctx context.Context, captureID int64, folder string) (model.DuplicateResult, error)
	// Latest returns the latest capture of one scope, filtered as requested.
	Latest(ctx context.Context, query LatestQuery) (Latest, error)
	// Compare ranks changes between two captures of a scope.
	Compare(ctx context.Context, query CompareQuery) (Comparison, error)
	// Capture measures a scope and saves it. Progress may be nil.
	Capture(ctx context.Context, request CaptureRequest, progress func(Progress)) (model.Snapshot, error)
	// Close releases the backend.
	Close() error
}

// Info identifies a backend.
type Info struct {
	Version   string
	IndexPath string
}

// LatestQuery selects a scope by scope ID, label or absolute folder and
// filters its records. Zero values select everything.
type LatestQuery struct {
	Scope  string
	Series string
	Query  string
	Limit  int
}

// Latest is the latest capture of a scope.
type Latest struct {
	Snapshot model.Snapshot
	// RefreshRecommended is true when drive or journal readings suggest the
	// capture is out of date.
	RefreshRecommended bool
}

// InventoryQuery filters the detailed file view. With Files false, Parent
// selects the direct children of a folder (empty means the scan root).
// With Files true, results include files at any depth.
type InventoryQuery struct {
	CaptureID     int64
	Parent        string
	Files         bool
	Query         string
	Within        string
	Extension     string
	MinimumBytes  int64
	ModifiedSince int64
	Removal       model.Removal
	Offset        int
	Limit         int
	SortColumn    int
	Ascending     bool
}

type InventoryPage struct {
	Items []model.Record
	Total int
}

const InventoryPageSize = 256

// Direction filters changes by sign.
type Direction int

// Directions.
const (
	DirectionBoth Direction = iota
	DirectionGrowth
	DirectionFreed
)

// CompareQuery selects a scope and a window and filters the ranked changes.
// Zero values select everything; Days 0 compares with the previous capture.
type CompareQuery struct {
	Scope        string
	Days         int
	Series       string
	MinimumBytes int64
	Direction    Direction
	Query        string
	Limit        int
	// FromID and ToID select an exact pair. Set both or leave both at zero.
	FromID int64
	ToID   int64
}

// MaxDays is the longest comparison window. History is kept for 120 days.
const MaxDays = 120

// Comparison is a comparison with the free-space movement of each drive.
type Comparison struct {
	model.Comparison
	Drives []model.DriveDelta
}

// CaptureRequest names the scope to measure: model.QuickScope,
// model.DockerScope, a fixed drive or an absolute local folder.
type CaptureRequest struct {
	Scope string
	// Kind optionally sets the category of a folder scope.
	Kind string
	// Exclusions are absolute paths skipped during this capture.
	Exclusions []string
	// Detail saves a paged inventory for the latest capture of the scope.
	Detail bool
}

// Progress reports how far a capture has come.
type Progress struct {
	FilesChecked int64
	// Saving is true once measurement is complete and the snapshot is being saved.
	Saving bool
}
