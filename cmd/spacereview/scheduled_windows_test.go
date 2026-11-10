package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/liljestk/VIbeClean/internal/schedule"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/service/local"
)

func TestHeadlessScheduledCapture(t *testing.T) {
	ctx := context.Background()
	root, index := t.TempDir(), filepath.Join(t.TempDir(), "history.sqlite3")
	if err := os.WriteFile(filepath.Join(root, "example.txt"), make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	config := schedule.Config{Scope: root, At: "08:00"}
	encoded, err := config.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := runScheduled(ctx, options{index: index, scheduled: encoded}); err != nil {
		t.Fatal(err)
	}
	outcome, err := (schedule.Manager{Index: index}).ReadOutcome(root)
	if err != nil || outcome.Attempted == 0 || outcome.LastSaved == 0 || outcome.Result != "Scan saved." {
		t.Fatalf("run status = %+v, %v", outcome, err)
	}
	backend, err := local.Open(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	latest, err := backend.Latest(ctx, service.LatestQuery{Scope: root})
	if err != nil || latest.Snapshot.Coverage.Checked != 1 || latest.Snapshot.Captured != outcome.LastSaved {
		t.Fatalf("scheduled capture = %+v, %v", latest, err)
	}
}
