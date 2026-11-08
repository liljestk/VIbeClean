package desktop

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liljestk/VIbeClean/internal/model"
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
