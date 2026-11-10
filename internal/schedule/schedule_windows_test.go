package schedule

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

func TestAlertRearmsAfterSpaceRecovers(t *testing.T) {
	config := Config{Scope: model.QuickScope, ThresholdGiB: 10}
	drive := model.DriveState{ID: "volume", Root: `C:\`, Total: 50 * units.GiB, Free: 10 * units.GiB}
	state := Outcome{}
	if len(state.LowSpace(config, []model.DriveState{drive})) != 1 ||
		len(state.LowSpace(config, []model.DriveState{drive})) != 0 {
		t.Fatal("first low reading should alert once")
	}
	drive.Free += units.GiB
	if len(state.LowSpace(config, []model.DriveState{drive})) != 0 || len(state.LowVolumes) != 0 {
		t.Fatal("recovery should rearm the alert")
	}
	drive.Free -= units.GiB
	if len(state.LowSpace(config, []model.DriveState{drive})) != 1 {
		t.Fatal("the next low episode should alert")
	}
}

func TestRegisterReadRemoveTask(t *testing.T) {
	if os.Getenv("SPACE_REVIEW_TEST_SCHEDULER") != "1" {
		t.Skip("set SPACE_REVIEW_TEST_SCHEDULER=1 to register a temporary task")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	portable := os.Getenv("SPACE_REVIEW_EXE")
	if portable != "" {
		exe = portable
	}
	manager := Manager{Executable: exe, Index: filepath.Join(t.TempDir(), "history.sqlite3")}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("a scheduled scan fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	config := Config{Scope: root, At: time.Now().Add(time.Hour).Format("15:04")}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := manager.Save(ctx, config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Remove(ctx, config.Scope); err != nil {
			t.Errorf("remove temporary task: %v", err)
		}
	})
	state, err := manager.Read(ctx, config.Scope)
	if err != nil || !state.Registered || !state.Enabled || state.Config != config || state.Problem != "" {
		t.Fatalf("task registration = %+v, %v", state, err)
	}
	name, _, err := manager.identity(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := scheduler(ctx, "/Query", "/TN", name, "/XML")
	if err != nil {
		t.Fatal(taskError("verify test task", raw, err))
	}
	task, err := readDefinition(raw)
	if err != nil {
		t.Fatal(err)
	}
	settings := task.Settings
	if !settings.IdleOnly || !settings.IdleStops || !settings.IdleRestarts ||
		!settings.BatteryBlocked || !settings.BatteryStops || settings.WakeToRun {
		t.Fatalf("unexpected background task settings: %+v", settings)
	}
	if portable == "" {
		return
	}
	// Verify the registered action without waiting for Windows to enter idle.
	if output, err := scheduler(ctx, "/Run", "/I", "/TN", name); err != nil {
		t.Fatal(taskError("start test task", output, err))
	}
	for ctx.Err() == nil {
		outcome, err := manager.ReadOutcome(root)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Attempted != 0 {
			if outcome.Result != "Scan saved." || outcome.LastSaved == 0 {
				t.Fatalf("scheduled executable failed: %+v", outcome)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("scheduled executable did not report its result")
}
