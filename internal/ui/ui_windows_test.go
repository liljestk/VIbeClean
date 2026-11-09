package ui

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

func TestDailyCaptureDueUsesElapsedTime(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	s := model.Snapshot{ID: 1, Captured: now.Add(-24 * time.Hour).UnixMilli()}
	if !dailyCaptureDue(s, now) || dailyCaptureDue(s, now.Add(-time.Millisecond)) ||
		dailyCaptureDue(model.Snapshot{}, now) {
		t.Fatal("daily capture should become due at 24 hours after a saved capture")
	}
}

func TestClosestFolderWalksUpToAnExistingFolder(t *testing.T) {
	root := t.TempDir()
	if got, err := closestFolder(filepath.Join(root, "gone", "deeper", "file.bin")); err != nil || got != root {
		t.Fatalf("closestFolder = %q, %v; want %q", got, err, root)
	}
	file := filepath.Join(root, "file.bin")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := closestFolder(file); err != nil || got != root {
		t.Fatalf("a file should open its folder: %q, %v", got, err)
	}
}

func TestChangeRowsMarkBoundedSides(t *testing.T) {
	grown := model.Record{Series: model.LargeFiles, Name: "big.bin", Path: `C:\big.bin`, Bytes: 100 << 20}
	row := changeRow(model.Change{Series: model.LargeFiles, After: &grown, Bytes: 36<<20 + 1, Bounded: true})
	if row.Before != -1 || row.After != grown.Bytes || row.Cells[2] != "<64 MiB or absent" || row.Cells[1] != "≥ 36.0 MiB ↑" {
		t.Fatalf("bounded row: %+v", row)
	}
	removed := model.Record{Series: model.Folders, Name: "old", Path: `C:\old`, Bytes: 1 << 30}
	row = changeRow(model.Change{Series: model.Folders, Before: &removed, Bytes: -(1 << 30)})
	if row.Before != removed.Bytes || row.After != 0 || row.Cells[1] != "−1.0 GiB" {
		t.Fatalf("removed row: %+v", row)
	}
}

func TestChangeMetricsSumDrives(t *testing.T) {
	c := `C:\`
	d := `D:\`
	folder := func(path string, bytes int64) model.Change {
		return model.Change{Series: model.Folders, After: &model.Record{Path: path}, Bytes: bytes}
	}
	diff := service.Comparison{
		Comparison: model.Comparison{
			Latest:  model.Snapshot{Coverage: model.Coverage{Drives: []model.DriveState{{Root: c}, {Root: d}}}},
			Changes: []model.Change{folder(`C:\a`, 3<<30), folder(`D:\b`, -(1 << 30))},
		},
		Drives: []model.DriveDelta{{Root: c, UsedDelta: 4 << 30, Free: 10 << 30}, {Root: d, UsedDelta: -(1 << 30), Free: 20 << 30}},
	}
	got := changeMetrics(diff)
	if got[0].value != "+3.0 GiB" || got[1].value != "+2.0 GiB" || got[2].value != "30.0 GiB" ||
		got[0].caption != "C: +4.0 GiB  ·  D: −1.0 GiB" {
		t.Fatalf("unexpected metrics: %+v", got)
	}
}

func TestSortKeepsSelectedItem(t *testing.T) {
	p := rankedPane{ctx: context.Background()}
	p.init([4]column{{title: "Folder"}, {title: "Change"}, {title: "Before"}, {title: "After"}}, true, "")
	p.show([]displayRow{
		{Item: model.Record{Series: model.Folders, Key: "a", Name: "a"}, Rank: 3, Cells: [4]string{"a"}},
		{Item: model.Record{Series: model.Folders, Key: "b", Name: "b"}, Rank: 9, Cells: [4]string{"b"}},
		{Item: model.Record{Series: model.Folders, Key: "c", Name: "c"}, Rank: 5, Cells: [4]string{"c"}},
	}, "", "")
	if row, _ := p.selected(); row.Item.Key != "b" {
		t.Fatalf("largest change should be selected first, got %q", row.Item.Key)
	}
	p.setCurrent(1)
	p.sortBy(0)
	if row, _ := p.selected(); row.Item.Key != "c" || p.items[p.rows[0]].Item.Key != "a" {
		t.Fatalf("sorting by name should keep %q selected, got %q", "c", row.Item.Key)
	}
	p.search("zzz", "test")
	if _, ok := p.selected(); ok || p.empty == "" {
		t.Fatal("a search without matches should clear the selection and explain why")
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1284311: "1,284,311"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestClaudeBundleLaunchesWithSelectedHistory(t *testing.T) {
	exe := os.Getenv("SPACE_REVIEW_EXE")
	if exe == "" {
		t.Skip("set SPACE_REVIEW_EXE to test the bundled executable")
	}
	dir := t.TempDir()
	index := filepath.Join(dir, "selected history.sqlite3")
	path := filepath.Join(dir, "Space Review.mcpb")
	if err := writeClaudeBundle(path, exe, []string{"--mcp", "--index", index}); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := fs.ReadFile(&reader.Reader, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Server struct {
			EntryPoint string `json:"entry_point"`
			Config     struct {
				Command string
				Args    []string
			} `json:"mcp_config"`
		}
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	server := manifest.Server
	if server.EntryPoint != bundleExecutable || server.Config.Command != "${__dirname}/"+server.EntryPoint {
		t.Fatalf("unexpected bundled server: %+v", server)
	}
	data, err = fs.ReadFile(&reader.Reader, server.EntryPoint)
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Clean(strings.Replace(server.Config.Command, "${__dirname}", dir, 1))
	if err := os.MkdirAll(filepath.Dir(command), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "bundle-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.CommandContext(ctx, command, server.Config.Args...)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("could not discover bundled tools: %v, %v", tools, err)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("bundle did not open the selected history: %v", err)
	}
}

func TestAnalysisDraftUsesFilteredRowsAndScanTimes(t *testing.T) {
	const parent = `C:\private\project`
	before, after := int64(1000), int64(2000)
	saved := model.Snapshot{ID: 2, Label: parent, Captured: after}
	diff := service.Comparison{Comparison: model.Comparison{Latest: saved, Baseline: model.Snapshot{ID: 1, Captured: before}}}
	items := []displayRow{
		{Item: model.Record{Name: "small.bin"}, Rank: 2, Cells: [4]string{"small.bin", "+2 B", "0 B", "2 B"}},
		{Item: model.Record{Name: parent + `\big.bin`}, Rank: 8, Cells: [4]string{"big.bin", "+8 B", "0 B", "8 B"}},
		{Item: model.Record{Name: "filtered.bin"}, Rank: 12},
	}
	prompt := analysisPrompt(saved, model.LargeFiles, items, []int{0, 1}, &diff)
	big, small := strings.Index(prompt, `"big.bin", +8 B, before 0 B, after 8 B`), strings.Index(prompt, `"small.bin"`)
	if big < 0 || small <= big || strings.Contains(prompt, parent) || strings.Contains(prompt, "filtered.bin") ||
		!strings.Contains(prompt, scanTime(before)) || !strings.Contains(prompt, scanTime(after)) {
		t.Fatalf("unexpected analysis draft: %s", prompt)
	}
	for _, dest := range [][2]string{{"codex://threads/new", "prompt"}, {"claude://claude.ai/new", "q"}} {
		question := prompt + "\nInclude build & test output, if measured."
		link, err := url.Parse(assistantLink(dest[0], dest[1], question))
		if err != nil || link.Query().Get(dest[1]) != question {
			t.Fatalf("draft was not preserved in %s: %v", dest[0], err)
		}
	}
}

func TestPowerShellSetupQuotesArguments(t *testing.T) {
	got := mcpSetupCommand("codex mcp add space-review --", `C:\Space Review\it's.exe`, []string{"--index", `C:\a‘b’.db`})
	want := `codex mcp add space-review -- 'C:\Space Review\it''s.exe' '--index' 'C:\a‘‘b’’.db'`
	if got != want {
		t.Fatalf("mcpSetupCommand = %s, want %s", got, want)
	}
}

func TestRemovalRowsAndSummary(t *testing.T) {
	s := model.Snapshot{ID: 1, Coverage: model.Coverage{ScopeKind: model.ScopeFiles, Labels: model.LabelsVersion, Checked: 1200}, Records: []model.Record{
		{Series: model.Folders, Key: "a", Name: "a", Bytes: 100},
		{Series: model.Categories, Key: model.CacheKind, Name: model.CacheKind, Kind: model.CacheKind, Bytes: 30},
		{Series: model.Categories, Key: model.BuildKind, Name: model.BuildKind, Kind: model.BuildKind, Bytes: 9},
		{Series: model.Categories, Key: model.GameKind, Name: model.GameKind, Kind: model.GameKind, Bytes: 60},
		{Series: model.Categories, Key: model.PersonalKind, Name: model.PersonalKind, Kind: model.PersonalKind, Bytes: 1},
		{Series: model.Candidates, Key: "c", Name: "cache", Kind: model.CacheKind, Bytes: 30, Detail: "Marked as a cache."},
		{Series: model.Candidates, Key: "d", Name: "Downloads", Kind: model.DownloadKind, Bytes: 5},
	}}
	rows := categoryRows(s, "")
	if len(rows) != 4 {
		t.Fatalf("category rows: %+v", rows)
	}
	for _, row := range rows {
		if row.Item.Kind == model.GameKind && (row.Cells[2] != "Uninstall" || row.Cells[3] != "60.0%" || row.Mark != model.Uninstall) {
			t.Fatalf("games row: %+v", row)
		}
	}
	if safe := categoryRows(s, model.SafeToClear); len(safe) != 2 {
		t.Fatalf("the removal filter should keep safe categories only: %+v", safe)
	}
	check := fileRows(s, model.Candidates, model.CheckFirst)
	if len(check) != 1 || check[0].Item.Name != "Downloads" || check[0].Mark != model.CheckFirst {
		t.Fatalf("check-first candidates: %+v", check)
	}
	all := fileRows(s, model.Candidates, "")
	if len(all) != 2 || !strings.HasPrefix(all[0].Detail, "Marked as a cache.") {
		t.Fatalf("a label's reason should lead the note: %+v", all)
	}
	if got := largestKinds(s, model.SafeToClear); got != "Cache & logs, Build output" {
		t.Fatalf("largestKinds = %q", got)
	}
	if shareText(1, 2000) != "<0.1%" || shareText(1, 0) != "" {
		t.Fatal("unexpected share text")
	}
	u := &ui{}
	u.files = s
	metrics := u.fileMetrics()
	if len(metrics) != 4 || metrics[1].label != "Safe to clear" || metrics[1].click == nil ||
		metrics[3].label != "Uninstall" || metrics[0].caption != "1,200 files" {
		t.Fatalf("unexpected summary: %+v", metrics)
	}
	u.files.Records = s.Records[:1]
	if metrics := u.fileMetrics(); len(metrics) != 3 || metrics[1].label != "Files checked" {
		t.Fatalf("captures without categories keep the earlier summary: %+v", metrics)
	}
}
