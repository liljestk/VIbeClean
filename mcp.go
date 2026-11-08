package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type scopeInfo struct {
	CaptureID    int64        `json:"capture_id"`
	Scope        string       `json:"scope_id"`
	Label        string       `json:"label"`
	Captured     string       `json:"captured_at"`
	Kind         string       `json:"kind"`
	FilesChecked int64        `json:"files_checked,omitempty"`
	Unreadable   int          `json:"unreadable_locations,omitempty"`
	Subfolders   bool         `json:"subfolders_indexed,omitempty"`
	Drives       []driveState `json:"drives,omitempty"`
	Engine       string       `json:"engine,omitempty"`
}

func snapshotInfo(s snapshot) scopeInfo {
	return scopeInfo{
		CaptureID: s.ID, Scope: s.Scope, Label: s.Label, Captured: time.UnixMilli(s.Captured).Format(time.RFC3339Nano),
		Kind: s.Coverage.ScopeKind, FilesChecked: s.Coverage.Checked,
		Unreadable: len(s.Coverage.Gaps), Subfolders: s.Coverage.Subfolders, Drives: s.Coverage.Drives, Engine: s.Coverage.Engine,
	}
}

type driveDelta struct {
	Root      string `json:"drive"`
	UsedDelta int64  `json:"used_delta_bytes"`
	Free      int64  `json:"free_bytes"`
}

func driveChanges(before, after []driveState) []driveDelta {
	var result []driveDelta
	for _, now := range after {
		for _, old := range before {
			if now.Root == old.Root && now.ID == old.ID && now.Total == old.Total {
				result = append(result, driveDelta{Root: now.Root, UsedDelta: old.Free - now.Free, Free: now.Free})
			}
		}
	}
	return result
}

func findScope(ctx context.Context, idx *index, name string) (snapshot, error) {
	if name == "" {
		name = quickScope
	}
	saved, err := idx.list(ctx)
	if err != nil {
		return snapshot{}, err
	}
	for _, item := range saved {
		if name == item.Scope || strings.EqualFold(name, item.Label) {
			return item, nil
		}
	}
	return snapshot{}, fmt.Errorf("no saved capture for %q. Capture that scope first", name)
}

func captureScopeType(scope string) string {
	switch {
	case strings.EqualFold(scope, dockerScope):
		return "docker"
	case scope == "" || strings.EqualFold(scope, quickScope):
		return "quick"
	case volumeRoot(scope) != "" && strings.EqualFold(strings.TrimRight(scope, "\\/"), strings.TrimRight(volumeRoot(scope), "\\")):
		return "drive"
	default:
		return "folder"
	}
}

func runCapture(ctx context.Context, idx *index, scope, kind string, progress func(string, int64)) (saved snapshot, err error) {
	started := time.Now()
	scopeType := captureScopeType(scope)
	stage := "measure"
	logDiagnostic(ctx, slog.LevelInfo, "capture.start", slog.String("scope_type", scopeType))
	defer func() {
		logDuration(ctx, "capture.finish", started, err, slog.String("scope_type", scopeType), slog.String("stage", stage),
			slog.Int64("capture_id", saved.ID), slog.Int64("files_checked", saved.Coverage.Checked))
	}()
	if strings.EqualFold(scope, dockerScope) {
		saved, err = captureDocker(ctx)
	} else {
		if scope == "" || strings.EqualFold(scope, quickScope) {
			scope = quickScope
		}
		saved, err = captureFiles(ctx, scope, kind, progress)
	}
	if err == nil {
		stage = "save"
		err = idx.save(ctx, &saved)
	}
	if saved.Coverage.Checked >= 500_000 {
		releaseStarted := time.Now()
		debug.FreeOSMemory()
		logDuration(ctx, "capture.memory_release", releaseStarted, nil)
	}
	return saved, err
}

func supportedSeries(kind string, comparing bool) []string {
	if kind == "docker" {
		if comparing {
			return []string{dockerSum, projects, volumes, containers, images, buildCache}
		}
		return []string{projects, volumes, containers, images, buildCache, dockerSum}
	}
	return []string{folders, subfolders, candidates, largeFiles}
}

func chooseSeries(kind, wanted string, comparing bool) (string, error) {
	valid := supportedSeries(kind, comparing)
	if wanted == "" {
		return valid[0], nil
	}
	for _, series := range valid {
		if strings.EqualFold(series, wanted) {
			return series, nil
		}
	}
	return "", fmt.Errorf("choose one of %s", strings.Join(valid, ", "))
}

func recordsIn(s snapshot, series string) []record {
	var items []record
	for _, item := range s.Records {
		if item.Series == series {
			items = append(items, item)
		}
	}
	slices.SortFunc(items, func(a, b record) int {
		if a.Bytes > b.Bytes {
			return -1
		}
		if a.Bytes < b.Bytes {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return items
}

func limitRows(limit int) int {
	if limit <= 0 {
		return 20
	}
	return min(limit, 100)
}

type latestInput struct {
	Scope  string `json:"scope,omitempty" jsonschema:"Saved scope ID or label. Defaults to Quick review."`
	Series string `json:"series,omitempty" jsonschema:"Folders, Subfolders, Review candidates, Large files, Projects, Volumes, Containers, Images, Build cache, or Docker totals."`
	Query  string `json:"query,omitempty" jsonschema:"Filter by words, or use path: followed by an absolute folder to filter its contents."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Number of rows, default 20, maximum 100."`
}

type latestOutput struct {
	Snapshot scopeInfo `json:"snapshot"`
	Series   string    `json:"series"`
	Items    []record  `json:"items"`
}

type changesInput struct {
	Scope     string `json:"scope,omitempty" jsonschema:"Saved scope ID or label. Defaults to Quick review."`
	Series    string `json:"series,omitempty" jsonschema:"Files: Folders, Subfolders, Review candidates, Large files. Docker: Docker totals, Projects, Volumes, Containers, Images, Build cache."`
	Days      int    `json:"days,omitempty" jsonschema:"0 compares the previous scan. Use 1, 7, 30, or 90 for longer periods."`
	MinMiB    int64  `json:"minimum_mib,omitempty" jsonschema:"Minimum absolute change, default 256 MiB."`
	Direction string `json:"direction,omitempty" jsonschema:"both, growth, or freed."`
	Query     string `json:"query,omitempty" jsonschema:"Filter by words, or use path: followed by an absolute folder to filter its contents."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Number of changes, default 20, maximum 100."`
}

type changesOutput struct {
	Latest      scopeInfo    `json:"latest"`
	Baseline    *scopeInfo   `json:"baseline,omitempty"`
	ShortWindow bool         `json:"short_window"`
	Omitted     int          `json:"omitted_coverage_changes"`
	Drives      []driveDelta `json:"drive_changes,omitempty"`
	Series      string       `json:"series"`
	Notice      string       `json:"notice,omitempty"`
	Changes     []change     `json:"changes"`
}

type captureInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"Quick review, Docker, a fixed drive like C:\\, or an absolute local folder."`
}

type listOutput struct {
	Scopes []scopeInfo `json:"scopes"`
}

func measureMCP(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		ctx = diagnosticOperation(ctx)
		defer logPanic(ctx, "mcp.request")
		started := time.Now()
		attrs := []slog.Attr{slog.String("method", method)}
		if params, ok := request.GetParams().(*mcp.CallToolParamsRaw); ok && params != nil {
			tool := "unknown"
			switch params.Name {
			case "list_storage_scopes", "largest_storage_items", "storage_changes", "capture_storage":
				tool = params.Name
			}
			attrs = append(attrs, slog.String("tool", tool))
		}
		result, err := next(ctx, method, request)
		reported := err
		if tool, ok := result.(*mcp.CallToolResult); ok && tool != nil && tool.IsError && reported == nil {
			reported = diagnosticCode("tool_error")
		}
		logDuration(ctx, "mcp.request", started, reported, attrs...)
		return result, err
	}
}

func runMCP(ctx context.Context, idx *index) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "space-review", Version: appVersion}, &mcp.ServerOptions{
		Instructions: "Local Windows disk and Docker usage. Compare captures of the same scope. " +
			"Drive deltas are actual free-space measurements. Docker categories and Windows disk allocations can overlap. " +
			"An absent large-file or subfolder reading is below the 64 MiB tracking cutoff or missing. Such changes are conservative bounds.",
	})
	server.AddReceivingMiddleware(measureMCP)
	read := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "list_storage_scopes", Description: "List saved file and Docker capture scopes.",
		Annotations: read}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOutput, error) {
		result := listOutput{Scopes: []scopeInfo{}}
		scopes, err := idx.list(ctx)
		for _, item := range scopes {
			result.Scopes = append(result.Scopes, snapshotInfo(item))
		}
		return nil, result, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "largest_storage_items", Description: "Read the largest entries in the latest saved capture.",
		Annotations: read}, func(ctx context.Context, _ *mcp.CallToolRequest, input latestInput) (*mcp.CallToolResult, latestOutput, error) {
		var result latestOutput
		scope, err := findScope(ctx, idx, input.Scope)
		if err != nil {
			return nil, result, err
		}
		series, err := chooseSeries(scope.Coverage.ScopeKind, input.Series, false)
		if err != nil {
			return nil, result, err
		}
		saved, err := idx.latest(ctx, scope.Scope)
		if err != nil {
			return nil, result, err
		}
		items := recordsIn(saved, series)
		filter := parseFilter(input.Query)
		items = slices.DeleteFunc(items, func(item record) bool {
			return !filter.matches(item, item.Detail)
		})
		result = latestOutput{Snapshot: snapshotInfo(saved), Series: series,
			Items: append([]record{}, items[:min(len(items), limitRows(input.Limit))]...)}
		return nil, result, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "storage_changes", Description: "Rank the biggest storage gains or losses between saved captures.",
		Annotations: read}, func(ctx context.Context, _ *mcp.CallToolRequest, input changesInput) (*mcp.CallToolResult, changesOutput, error) {
		var result changesOutput
		if input.Days < 0 || input.Days > 120 || input.MinMiB < 0 || input.MinMiB > 1<<40 {
			return nil, result, errors.New("choose a window from 0 to 120 days and a valid size threshold")
		}
		switch input.Direction {
		case "", "both", "growth", "freed":
		default:
			return nil, result, errors.New("direction must be both, growth, or freed")
		}
		scope, err := findScope(ctx, idx, input.Scope)
		if err != nil {
			return nil, result, err
		}
		series, err := chooseSeries(scope.Coverage.ScopeKind, input.Series, true)
		if err != nil {
			return nil, result, err
		}
		diff, err := idx.compare(ctx, scope.Scope, input.Days)
		if err != nil {
			return nil, result, err
		}
		result = changesOutput{Latest: snapshotInfo(diff.Latest), Series: series, ShortWindow: diff.ShortWindow,
			Omitted: diff.Omitted, Changes: []change{}}
		if diff.Baseline.ID != 0 {
			info := snapshotInfo(diff.Baseline)
			result.Baseline = &info
			result.Drives = driveChanges(diff.Baseline.Coverage.Drives, diff.Latest.Coverage.Drives)
		}
		if series == subfolders && diff.Baseline.ID != 0 &&
			(!diff.Baseline.Coverage.Subfolders || !diff.Latest.Coverage.Subfolders) {
			result.Notice = "Subfolder changes require a baseline and a latest capture that both index subfolders."
			return nil, result, nil
		}
		if series == images || series == buildCache {
			result.Notice = "Individual Docker entries can share data. Use Docker totals to compare category usage."
		}
		minimum := input.MinMiB * mib
		if minimum == 0 {
			minimum = minChange
		}
		filter := parseFilter(input.Query)
		for _, item := range diff.Changes {
			if item.Series == series && magnitude(item.Bytes) >= minimum &&
				(input.Direction != "growth" || item.Bytes > 0) && (input.Direction != "freed" || item.Bytes < 0) &&
				filter.matches(item.item(), item.item().Detail) {
				result.Changes = append(result.Changes, item)
				if len(result.Changes) == limitRows(input.Limit) {
					break
				}
			}
		}
		return nil, result, nil
	})
	captureGate := make(chan struct{}, 1)
	no := false
	mcp.AddTool(server, &mcp.Tool{Name: "capture_storage", Description: "Measure a local file or Docker scope and save a SQLite snapshot. Reads storage metadata.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &no}},
		func(ctx context.Context, _ *mcp.CallToolRequest, input captureInput) (*mcp.CallToolResult, scopeInfo, error) {
			select {
			case captureGate <- struct{}{}:
				defer func() { <-captureGate }()
			case <-ctx.Done():
				return nil, scopeInfo{}, ctx.Err()
			}
			saved, err := runCapture(ctx, idx, input.Scope, "", nil)
			if err != nil {
				return nil, scopeInfo{}, err
			}
			return nil, snapshotInfo(saved), nil
		})
	return server.Run(ctx, &mcp.StdioTransport{})
}
