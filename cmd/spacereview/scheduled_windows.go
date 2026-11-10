package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/schedule"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/service/local"
	"github.com/liljestk/VIbeClean/internal/units"
)

func runScheduled(ctx context.Context, opts options) error {
	config, err := schedule.Decode(opts.scheduled)
	if err != nil {
		return err
	}
	manager := schedule.Manager{Index: opts.index}
	state, err := manager.ReadOutcome(config.Scope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Hour+50*time.Minute)
	defer cancel()
	state.Attempted = time.Now().UnixMilli()
	saved, drives, err := scheduledCapture(ctx, opts.index, config.Scope)
	switch {
	case errors.Is(err, local.ErrCaptureBusy):
		state.Result = "Skipped because another scan was running."
		return manager.WriteOutcome(config.Scope, state)
	case err != nil:
		state.Result = "Scan failed. " + err.Error()
		return errors.Join(err, manager.WriteOutcome(config.Scope, state))
	}
	state.LastSaved, state.Result = saved.Captured, "Scan saved."
	crossings := state.LowSpace(config, drives)
	if len(state.LowVolumes) == 0 {
		state.Alert, state.AlertProblem = "", ""
	}
	if len(crossings) > 0 {
		var readings []string
		for _, disk := range crossings {
			readings = append(readings, filepath.VolumeName(disk.Root)+" has "+units.Size(disk.Free)+" free")
		}
		state.Alert, state.AlertProblem = strings.Join(readings, " · "), ""
	}
	// Record the transition before showing the notification, so restarting a
	// job cannot repeat an alert for the same low-space episode.
	if err := manager.WriteOutcome(config.Scope, state); err != nil {
		return err
	}
	if len(crossings) > 0 {
		if err := schedule.Notify(ctx, "Space Review · Low disk space", state.Alert); err != nil {
			state.AlertProblem = err.Error()
			return manager.WriteOutcome(config.Scope, state)
		}
	}
	return nil
}

func scheduledCapture(ctx context.Context, index, scope string) (model.Snapshot, []model.DriveState, error) {
	process := windows.CurrentProcess()
	if err := windows.SetPriorityClass(process, windows.PROCESS_MODE_BACKGROUND_BEGIN); err == nil {
		defer windows.SetPriorityClass(process, windows.PROCESS_MODE_BACKGROUND_END)
	} else if !errors.Is(err, windows.ERROR_PROCESS_MODE_ALREADY_BACKGROUND) {
		return model.Snapshot{}, nil, fmt.Errorf("set background CPU and disk I/O priority: %w", err)
	}
	backend, err := local.Open(ctx, index)
	if err != nil {
		return model.Snapshot{}, nil, err
	}
	defer backend.Close()
	latest, err := backend.Latest(ctx, service.LatestQuery{Scope: scope, Limit: 1})
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return model.Snapshot{}, nil, err
	}
	settings := latest.Snapshot.Coverage
	saved, err := backend.CaptureIfIdle(ctx, service.CaptureRequest{
		Scope: scope, Kind: settings.Kind, Exclusions: settings.Exclusions, Detail: settings.Inventory,
	})
	if err != nil {
		return model.Snapshot{}, nil, err
	}
	drives := saved.Coverage.Drives
	if saved.Coverage.ScopeKind == model.ScopeDocker {
		drives, _ = backend.Drives(ctx)
	}
	return saved, drives, nil
}
