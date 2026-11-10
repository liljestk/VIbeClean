// Package mcpserver exposes the backend as Model Context Protocol tools over
// standard input and output. The tools mirror the gRPC API, so assistants and
// user interfaces see the same data.
package mcpserver

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

// Name is the MCP server name.
const Name = "space-review"

// Tool names.
const (
	ToolListScopes = "list_storage_scopes"
	ToolLargest    = "largest_storage_items"
	ToolChanges    = "storage_changes"
	ToolCapture    = "capture_storage"
	ToolDrives     = "list_drives"
	ToolBreakdown  = "storage_breakdown"
)

const (
	defaultRows      = 20
	defaultBreakdown = 10
	maxRows          = 100
	maxMinMiB        = 1 << 40
)

// ScopeInfo summarizes one capture.
type ScopeInfo struct {
	CaptureID    int64              `json:"capture_id"`
	Scope        string             `json:"scope_id"`
	Label        string             `json:"label"`
	Captured     string             `json:"captured_at"`
	Kind         string             `json:"kind"`
	FilesChecked int64              `json:"files_checked,omitempty"`
	Unreadable   int                `json:"unreadable_locations,omitempty"`
	Subfolders   bool               `json:"subfolders_indexed,omitempty"`
	Drives       []model.DriveState `json:"drives,omitempty"`
	Engine       string             `json:"engine,omitempty"`
}

func scopeInfo(s model.Snapshot) ScopeInfo {
	return ScopeInfo{
		CaptureID: s.ID, Scope: s.Scope, Label: s.Label, Captured: time.UnixMilli(s.Captured).Format(time.RFC3339Nano),
		Kind: s.Coverage.ScopeKind, FilesChecked: s.Coverage.Checked,
		Unreadable: len(s.Coverage.Gaps), Subfolders: s.Coverage.Subfolders, Drives: s.Coverage.Drives, Engine: s.Coverage.Engine,
	}
}

// LatestInput selects the latest records of a scope.
type LatestInput struct {
	Scope  string `json:"scope,omitempty" jsonschema:"Saved scope ID, label or absolute folder. Defaults to Quick review."`
	Series string `json:"series,omitempty" jsonschema:"Files: Folders, Subfolders, Categories, Review candidates, Apps, games & projects, or Large files. Docker: Projects, Volumes, Containers, Images, Build cache, or Docker totals."`
	Query  string `json:"query,omitempty" jsonschema:"Filter by words, or use path: followed by an absolute folder to filter its contents."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Number of rows, default 20, maximum 100."`
}

// Item is one reading. File readings carry the removal level of their category.
type Item struct {
	model.Record
	Removal model.Removal `json:"removal,omitempty"`
}

func fileItems(records []model.Record, coverage model.Coverage, limit int) []Item {
	result := make([]Item, 0, min(len(records), limit))
	for _, record := range records[:min(len(records), limit)] {
		item := Item{Record: record}
		if coverage.ScopeKind == model.ScopeFiles && record.Kind != "" {
			item.Removal = model.CaptureRemoval(record.Kind, coverage.Labels)
		}
		result = append(result, item)
	}
	return result
}

// LatestOutput lists the largest records of one series.
type LatestOutput struct {
	Snapshot           ScopeInfo `json:"snapshot"`
	Series             string    `json:"series"`
	RefreshRecommended bool      `json:"refresh_recommended,omitempty"`
	Items              []Item    `json:"items"`
}

// BreakdownInput selects the file capture to summarize.
type BreakdownInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"Saved scope ID, label or absolute folder. Defaults to Quick review. A drive capture gives the fullest picture."`
	Limit int    `json:"limit,omitempty" jsonschema:"Number of review candidates and labeled folders to list, default 10, maximum 100."`
}

// RemovalTotal is the space of every category with one removal level.
type RemovalTotal struct {
	Removal model.Removal `json:"removal"`
	Bytes   int64         `json:"bytes"`
	Note    string        `json:"note"`
}

// CategoryTotal is the space one category uses. Each file counts toward one category.
type CategoryTotal struct {
	Category string        `json:"category"`
	Bytes    int64         `json:"bytes"`
	Files    int64         `json:"files"`
	Removal  model.Removal `json:"removal"`
	Note     string        `json:"note"`
}

// BreakdownOutput summarizes a file capture by removal level and category.
type BreakdownOutput struct {
	Snapshot           ScopeInfo       `json:"snapshot"`
	RefreshRecommended bool            `json:"refresh_recommended,omitempty"`
	Notice             string          `json:"notice,omitempty"`
	Removals           []RemovalTotal  `json:"removal_totals"`
	Categories         []CategoryTotal `json:"categories"`
	Candidates         []Item          `json:"largest_review_candidates"`
	Labeled            []Item          `json:"largest_apps_games_projects"`
}

// ChangesInput selects and filters a comparison.
type ChangesInput struct {
	Scope     string `json:"scope,omitempty" jsonschema:"Saved scope ID, label or absolute folder. Defaults to Quick review."`
	Series    string `json:"series,omitempty" jsonschema:"Files: Folders, Subfolders, Review candidates, Apps, games & projects, Large files. Docker: Docker totals, Projects, Volumes, Containers, Images, Build cache."`
	Days      int    `json:"days,omitempty" jsonschema:"0 compares the previous scan. Use 1, 7, 30, or 90 for longer periods, up to 120."`
	MinMiB    int64  `json:"minimum_mib,omitempty" jsonschema:"Minimum absolute change, default 256 MiB."`
	Direction string `json:"direction,omitempty" jsonschema:"both, growth, or freed."`
	Query     string `json:"query,omitempty" jsonschema:"Filter by words, or use path: followed by an absolute folder to filter its contents."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Number of changes, default 20, maximum 100."`
}

// ChangesOutput holds a ranked comparison.
type ChangesOutput struct {
	Latest      ScopeInfo          `json:"latest"`
	Baseline    *ScopeInfo         `json:"baseline,omitempty"`
	ShortWindow bool               `json:"short_window"`
	Omitted     int                `json:"omitted_coverage_changes"`
	Drives      []model.DriveDelta `json:"drive_changes,omitempty"`
	Series      string             `json:"series"`
	Notice      string             `json:"notice,omitempty"`
	Changes     []model.Change     `json:"changes"`
}

// CaptureInput names the scope to capture.
type CaptureInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"Quick review, Docker, a fixed drive like C:\\, or an absolute local folder."`
}

// ListOutput lists saved scopes.
type ListOutput struct {
	Scopes []ScopeInfo `json:"scopes"`
}

// DrivesOutput lists the current fixed-drive readings.
type DrivesOutput struct {
	Drives []model.DriveState `json:"drives"`
}

func limitRows(limit int) int {
	if limit <= 0 {
		return defaultRows
	}
	return min(limit, maxRows)
}

func direction(value string) (service.Direction, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "both":
		return service.DirectionBoth, nil
	case "growth":
		return service.DirectionGrowth, nil
	case "freed":
		return service.DirectionFreed, nil
	}
	return 0, model.InvalidArgument("direction must be both, growth, or freed")
}

// measure times each request and records which known tool it called. Tool
// arguments are never logged because they can contain paths and search text.
func measure(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		ctx = diag.Operation(ctx)
		defer diag.RecoverPanic(ctx, "mcp.request")
		started := time.Now()
		attrs := []slog.Attr{slog.String("method", method)}
		if params, ok := request.GetParams().(*mcp.CallToolParamsRaw); ok && params != nil {
			tool := "unknown"
			switch params.Name {
			case ToolListScopes, ToolLargest, ToolChanges, ToolCapture, ToolDrives, ToolBreakdown:
				tool = params.Name
			}
			attrs = append(attrs, slog.String("tool", tool))
		}
		result, err := next(ctx, method, request)
		reported := err
		if tool, ok := result.(*mcp.CallToolResult); ok && tool != nil && tool.IsError && reported == nil {
			reported = diag.Code("tool_error")
		}
		diag.Duration(ctx, "mcp.request", started, reported, attrs...)
		return result, err
	}
}

// New builds the MCP server over a backend.
func New(backend service.Backend) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: Name, Version: buildinfo.Version}, &mcp.ServerOptions{
		Instructions: "Use list_drives for live free space and largest_storage_items for saved folder usage or large files. Report the scan time. " +
			"For coding-session storage changes, scan the same project folder before and after. storage_changes with days=0 compares the last two captures. Report both times and check they cover the requested session. " +
			"Drive deltas are actual free-space measurements. Docker categories and Windows disk allocations can overlap. " +
			"An absent large-file or subfolder reading is below the 64 MiB tracking cutoff or missing. Such changes are conservative bounds. " +
			"For cleanup questions, use storage_breakdown on a drive capture. Categories and removal levels are heuristics based on paths and marker files. " +
			"Lead with Safe to clear, ask the user to look inside Check first items, and suggest uninstalling programs and games instead of deleting their folders. Never suggest deleting Keep items.",
	})
	server.AddReceivingMiddleware(measure)
	tools := &toolset{backend: backend}
	read := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: ToolListScopes, Description: "List saved file and Docker capture scopes.", Annotations: read}, tools.listScopes)
	mcp.AddTool(server, &mcp.Tool{Name: ToolDrives, Description: "Read current free space and identity of each local fixed drive.", Annotations: read}, tools.listDrives)
	mcp.AddTool(server, &mcp.Tool{Name: ToolLargest, Description: "Read the largest entries in the latest saved capture.", Annotations: read}, tools.largest)
	mcp.AddTool(server, &mcp.Tool{Name: ToolChanges, Description: "Rank the biggest storage gains or losses between saved captures.", Annotations: read}, tools.changes)
	mcp.AddTool(server, &mcp.Tool{Name: ToolBreakdown, Description: "Summarize the latest file capture by category and removal level " +
		"(Safe to clear, Check first, Uninstall, Keep), with the largest cleanup candidates and the largest programs, games and projects.",
		Annotations: read}, tools.breakdown)
	no := false
	mcp.AddTool(server, &mcp.Tool{Name: ToolCapture, Description: "Measure a local file or Docker scope and save a SQLite snapshot. Reads storage metadata.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &no}}, tools.capture)
	return server
}

// Run serves MCP over standard input and output until the client disconnects.
func Run(ctx context.Context, backend service.Backend) error {
	return New(backend).Run(ctx, &mcp.StdioTransport{})
}

type toolset struct {
	backend service.Backend
}

func (t *toolset) listScopes(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListOutput, error) {
	result := ListOutput{Scopes: []ScopeInfo{}}
	scopes, err := t.backend.Scopes(ctx)
	for _, item := range scopes {
		result.Scopes = append(result.Scopes, scopeInfo(item))
	}
	return nil, result, err
}

func (t *toolset) listDrives(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, DrivesOutput, error) {
	drives, err := t.backend.Drives(ctx)
	return nil, DrivesOutput{Drives: append([]model.DriveState{}, drives...)}, err
}

func (t *toolset) largest(ctx context.Context, _ *mcp.CallToolRequest, input LatestInput) (*mcp.CallToolResult, LatestOutput, error) {
	latest, err := t.backend.Latest(ctx, service.LatestQuery{Scope: input.Scope, Series: input.Series, Query: input.Query})
	if err != nil {
		return nil, LatestOutput{}, err
	}
	series, err := model.ChooseSeries(latest.Snapshot.Coverage.ScopeKind, input.Series, false)
	if err != nil {
		return nil, LatestOutput{}, err
	}
	items := model.RecordsIn(latest.Snapshot, series)
	return nil, LatestOutput{Snapshot: scopeInfo(latest.Snapshot), Series: series, RefreshRecommended: latest.RefreshRecommended,
		Items: fileItems(items, latest.Snapshot.Coverage, limitRows(input.Limit))}, nil
}

func (t *toolset) breakdown(ctx context.Context, _ *mcp.CallToolRequest, input BreakdownInput) (*mcp.CallToolResult, BreakdownOutput, error) {
	latest, err := t.backend.Latest(ctx, service.LatestQuery{Scope: input.Scope})
	if err != nil {
		return nil, BreakdownOutput{}, err
	}
	s := latest.Snapshot
	if s.Coverage.ScopeKind != model.ScopeFiles {
		return nil, BreakdownOutput{}, model.InvalidArgument("storage_breakdown covers file captures; use largest_storage_items for Docker")
	}
	limit := defaultBreakdown
	if input.Limit > 0 {
		limit = min(input.Limit, maxRows)
	}
	result := BreakdownOutput{Snapshot: scopeInfo(s), RefreshRecommended: latest.RefreshRecommended,
		Removals: []RemovalTotal{}, Categories: []CategoryTotal{},
		Candidates: fileItems(model.RecordsIn(s, model.Candidates), s.Coverage, limit),
		Labeled:    fileItems(model.RecordsIn(s, model.Labeled), s.Coverage, limit)}
	totals, ok := model.RemovalTotals(s)
	if !ok {
		result.Notice = "This capture has no category totals. Capture the scope again to label its contents."
		return nil, result, nil
	}
	if s.Coverage.Labels < model.LabelsVersion {
		result.Notice = "Capture the scope again to apply the current cleanup rules."
	}
	for _, removal := range model.Removals {
		result.Removals = append(result.Removals, RemovalTotal{Removal: removal, Bytes: totals[removal], Note: model.RemovalNote(removal)})
	}
	for _, item := range model.RecordsIn(s, model.Categories) {
		result.Categories = append(result.Categories, CategoryTotal{Category: item.Kind, Bytes: item.Bytes, Files: item.Files,
			Removal: model.CaptureRemoval(item.Kind, s.Coverage.Labels), Note: model.KindNote(item.Kind)})
	}
	return nil, result, nil
}

func (t *toolset) changes(ctx context.Context, _ *mcp.CallToolRequest, input ChangesInput) (*mcp.CallToolResult, ChangesOutput, error) {
	var result ChangesOutput
	if input.MinMiB < 0 || input.MinMiB > maxMinMiB {
		return nil, result, model.InvalidArgument("choose a minimum change from 0 to %d MiB", int64(maxMinMiB))
	}
	way, err := direction(input.Direction)
	if err != nil {
		return nil, result, err
	}
	minimum := input.MinMiB * units.MiB
	if minimum == 0 {
		minimum = model.MinChange
	}
	diff, err := t.backend.Compare(ctx, service.CompareQuery{Scope: input.Scope, Days: input.Days, Series: input.Series,
		MinimumBytes: minimum, Direction: way, Query: input.Query})
	if err != nil {
		return nil, result, err
	}
	series, err := model.ChooseSeries(diff.Latest.Coverage.ScopeKind, input.Series, true)
	if err != nil {
		return nil, result, err
	}
	result = ChangesOutput{Latest: scopeInfo(diff.Latest), Series: series, ShortWindow: diff.ShortWindow,
		Omitted: diff.Omitted, Changes: []model.Change{}}
	if diff.Baseline.ID != 0 {
		info := scopeInfo(diff.Baseline)
		result.Baseline = &info
		result.Drives = diff.Drives
	}
	if series == model.Subfolders && diff.Baseline.ID != 0 &&
		(!diff.Baseline.Coverage.Subfolders || !diff.Latest.Coverage.Subfolders) {
		result.Notice = "Subfolder changes require a baseline and a latest capture that both index subfolders."
		return nil, result, nil
	}
	if series == model.Images || series == model.BuildCache {
		result.Notice = "Individual Docker entries can share data. Use Docker totals to compare category usage."
	}
	if (series == model.Candidates || series == model.Labeled) && diff.Baseline.ID != 0 {
		result.Notice = "Only entries measured in the same category under the same rules in both captures are comparable."
	}
	for _, item := range diff.Changes {
		if item.Series == series {
			result.Changes = append(result.Changes, item)
			if len(result.Changes) == limitRows(input.Limit) {
				break
			}
		}
	}
	return nil, result, nil
}

func (t *toolset) capture(ctx context.Context, _ *mcp.CallToolRequest, input CaptureInput) (*mcp.CallToolResult, ScopeInfo, error) {
	saved, err := t.backend.Capture(ctx, service.CaptureRequest{Scope: input.Scope}, nil)
	if err != nil {
		return nil, ScopeInfo{}, err
	}
	return nil, scopeInfo(saved), nil
}
