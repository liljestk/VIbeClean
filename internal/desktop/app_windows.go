// Package desktop is the native Windows user interface. It depends only on
// the service.Backend contract, so it runs the same against the in-process
// backend or a remote one reached over the gRPC API.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

const (
	tabChanges = iota
	tabFiles
	tabDocker
	tabMCP
)

const (
	selectPrompt     = "Select an item to see its details."
	slowRender       = 32 * time.Millisecond
	scheduleInterval = 15 * time.Minute
	dockerFreshness  = 15 * time.Minute
	dailyInterval    = 24 * time.Hour
	historyDepth     = 8
	swShowNormal     = 1
)

// Options configure the desktop app.
type Options struct {
	// Open connects to the backend. It runs on a worker so the window can
	// appear first; the app closes the backend when it exits.
	Open func(context.Context) (service.Backend, error)
	// Location describes where history is kept, shown on the MCP tab.
	Location string
	// MCPCommand and MCPArgs start an MCP server that uses the same history.
	MCPCommand string
	MCPArgs    []string
	// DiagnosticsDir is opened from the Help menu; empty when logging is off.
	DiagnosticsDir string
}

type captureTask struct {
	scope    string
	ctx      context.Context
	started  time.Time
	cancel   context.CancelFunc
	stopping bool
}

type guiRead struct{ cancel context.CancelFunc }

func cancelGUIRead(slot **guiRead) {
	if current := *slot; current != nil {
		*slot = nil
		current.cancel()
	}
}

// readForGUI runs a read on a worker and applies its result on the UI
// thread. Only the UI thread owns the slots, and a newer read in the same
// slot discards the result of an older one.
func readForGUI[T any](u *gui, slot **guiRead, event string, read func(context.Context) (T, error), apply func(T, error)) {
	cancelGUIRead(slot)
	ctx, cancel := context.WithCancel(diag.Operation(u.ctx))
	request := &guiRead{cancel: cancel}
	*slot = request
	started := time.Now()
	go func() {
		defer diag.RecoverPanic(ctx, event)
		value, err := read(ctx)
		if ctx.Err() != nil {
			diag.Duration(ctx, event, started, ctx.Err())
			return
		}
		queued := time.Now()
		u.dispatch(func() {
			if *slot != request || ctx.Err() != nil {
				diag.Duration(ctx, event, started, context.Canceled)
				return
			}
			*slot = nil
			defer cancel()
			applying := time.Now()
			apply(value, err)
			diag.Duration(ctx, event, started, err, slog.Float64("ui_queue_ms", diag.Milliseconds(applying.Sub(queued))),
				slog.Float64("apply_ms", diag.Milliseconds(time.Since(applying))))
		})
	}()
}

type gui struct {
	opts         Options
	backend      service.Backend
	opening      bool
	opened       <-chan service.Backend
	ctx          context.Context
	cancel       context.CancelFunc
	dispatchMu   sync.Mutex
	window       *walk.MainWindow
	tabs         *walk.TabWidget
	driveStrip   *walk.Composite
	driveLabels  []*walk.Label
	driveBars    []*walk.ProgressBar
	drives       []string
	activity     *walk.Label
	activityBar  *walk.ProgressBar
	cancelButton *walk.PushButton
	captureMenu  *walk.Action
	cancelMenu   *walk.Action
	findMenu     *walk.Action
	reloadMenu   *walk.Action
	active       *captureTask
	dueLoad      *guiRead

	filesTab
	dockerTab
	changesTab
}

func (u *gui) dispatch(apply func()) {
	u.dispatchMu.Lock()
	defer u.dispatchMu.Unlock()
	if u.ctx.Err() == nil {
		u.window.Synchronize(func() {
			if u.ctx.Err() == nil {
				apply()
			}
		})
	}
}

func (u *gui) stopDispatch() {
	u.dispatchMu.Lock()
	u.cancel()
	u.dispatchMu.Unlock()
}

// Run shows the window until the user closes it.
func Run(parent context.Context, opts Options) (err error) {
	started := time.Now()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ui := &gui{opts: opts, ctx: ctx, cancel: cancel}
	ui.files = model.Snapshot{Label: model.QuickScope}
	ui.filePane.ctx, ui.dockerPane.ctx, ui.changePane.ctx = ctx, ctx, ctx
	mcpPage, err := ui.mcpPage()
	if err != nil {
		return err
	}
	err = (d.MainWindow{
		AssignTo: &ui.window, Title: "Space Review",
		Size: d.Size{Width: 1160, Height: 780}, MinSize: d.Size{Width: 935, Height: 670},
		Font:      d.Font{Family: "Segoe UI", PointSize: 11},
		Layout:    d.VBox{Margins: d.Margins{Left: 16, Top: 14, Right: 16, Bottom: 12}, Spacing: 9},
		MenuItems: ui.menu(),
		Children: []d.Widget{
			d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
				d.Label{Text: "Space Review", Font: d.Font{PointSize: 22, Bold: true}},
				d.HSpacer{}, d.Label{Text: "Local history  ·  Read-only scans"},
			}},
			d.Composite{AssignTo: &ui.driveStrip, Layout: d.HBox{MarginsZero: true, Spacing: 8}},
			d.TabWidget{AssignTo: &ui.tabs, StretchFactor: 1, OnCurrentIndexChanged: ui.tabChanged,
				Pages: []d.TabPage{ui.changesPage(), ui.filesPage(), ui.dockerPage(), mcpPage}},
			d.Composite{Layout: d.HBox{MarginsZero: true, Spacing: 8}, Children: []d.Widget{
				d.Label{AssignTo: &ui.activity, Text: "Opening saved history…", NoPrefix: true, EllipsisMode: d.EllipsisEnd, StretchFactor: 1},
				d.ProgressBar{AssignTo: &ui.activityBar, Visible: false, MinSize: d.Size{Width: 110, Height: 8},
					MaxSize: d.Size{Width: 110, Height: 8}, Accessibility: d.Accessibility{Name: "Capture progress"}},
				d.PushButton{AssignTo: &ui.cancelButton, Text: "Cancel capture", Visible: false, OnClicked: ui.cancelActive},
			}},
		},
	}).Create()
	diag.Duration(ctx, "gui.window_create", started, err)
	if err != nil {
		return err
	}
	for _, pane := range []*rankedPane{&ui.changePane, &ui.filePane, &ui.dockerPane} {
		pane.table.SetRestoringCurrentItemOnReset(true)
		pane.table.ColumnClicked().Attach(func(_ int) { pane.showDetail() })
	}
	ui.window.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		diag.Log(ctx, slog.LevelInfo, "gui.close", slog.Bool("capture_active", ui.active != nil))
		ui.stopDispatch()
	})
	ui.renderFiles()
	ui.renderDocker()
	ui.renderChanges()
	ui.openBackend()
	go func() {
		defer diag.RecoverPanic(ctx, "gui.scheduler")
		ui.scheduleCaptures()
	}()
	ui.window.Run()
	ui.stopDispatch()
	if backend := <-ui.opened; backend != nil {
		_ = backend.Close()
	}
	return nil
}

func (u *gui) menu() []d.MenuItem {
	return []d.MenuItem{
		d.Menu{Text: "&File", Items: []d.MenuItem{
			d.Action{AssignTo: &u.captureMenu, Text: "Capture &now", Shortcut: d.Shortcut{Key: walk.KeyF5}, OnTriggered: u.captureCurrent},
			d.Action{AssignTo: &u.cancelMenu, Text: "&Cancel capture", Enabled: false, OnTriggered: u.cancelActive},
		}},
		d.Menu{Text: "&View", Items: []d.MenuItem{
			d.Action{AssignTo: &u.findMenu, Text: "&Find", Shortcut: d.Shortcut{Modifiers: walk.ModControl, Key: walk.KeyF}, OnTriggered: u.focusSearch},
			d.Action{AssignTo: &u.reloadMenu, Text: "Reload &history", OnTriggered: u.updateScopes},
		}},
		d.Menu{Text: "&Help", Items: []d.MenuItem{
			d.Action{Text: "Open &diagnostics folder", Enabled: u.opts.DiagnosticsDir != "", OnTriggered: func() {
				u.openFolder(u.opts.DiagnosticsDir, "gui.open_diagnostics", "Diagnostics folder unavailable", false)
			}},
		}},
	}
}

// openBackend connects on a worker so the window shows while history loads.
func (u *gui) openBackend() {
	if u.backend != nil || u.opening {
		return
	}
	u.opening = true
	u.setActivity("Opening saved history…")
	u.updateActions()
	opened := make(chan service.Backend, 1)
	u.opened = opened
	go u.loadStartup(opened)
}

type startup struct {
	backend service.Backend
	disks   []model.DriveState
	scopes  []model.Snapshot
	files   model.Snapshot
	docker  model.Snapshot
	refresh bool
}

func (u *gui) loadStartup(opened chan<- service.Backend) {
	defer diag.RecoverPanic(u.ctx, "gui.startup")
	started := time.Now()
	backend, err := u.opts.Open(u.ctx)
	if err != nil {
		backend = nil
	}
	opened <- backend
	if u.ctx.Err() != nil {
		return
	}
	loaded := startup{backend: backend, files: model.Snapshot{Label: model.QuickScope}, refresh: true}
	if err == nil {
		err = loaded.read(u.ctx)
	}
	if u.ctx.Err() != nil {
		return
	}
	u.dispatch(func() { u.applyStartup(loaded, started, err) })
}

func (s *startup) read(ctx context.Context) error {
	var err error
	if s.disks, err = s.backend.Drives(ctx); err != nil {
		return err
	}
	if s.scopes, err = s.backend.Scopes(ctx); err != nil {
		return err
	}
	if latest, err := s.backend.Latest(ctx, service.LatestQuery{Scope: model.QuickScope}); err == nil {
		s.files, s.refresh = latest.Snapshot, latest.RefreshRecommended
	}
	if latest, err := s.backend.Latest(ctx, service.LatestQuery{Scope: model.DockerScope}); err == nil {
		s.docker = latest.Snapshot
	}
	return nil
}

func (u *gui) applyStartup(loaded startup, started time.Time, err error) {
	u.opening = false
	u.backend, u.files, u.docker = loaded.backend, loaded.files, loaded.docker
	u.showDrives(loaded.disks)
	u.renderFiles()
	u.renderDocker()
	u.applyScopes(loaded.scopes)
	u.updateActions()
	diag.Duration(u.ctx, "gui.startup", started, err, slog.Bool("saved_files", loaded.files.ID != 0),
		slog.Bool("saved_docker", loaded.docker.ID != 0), slog.Int("fixed_drives", len(loaded.disks)))
	if err != nil {
		u.setActivity("Could not load saved history. Use View → Reload history to retry. " + err.Error())
		return
	}
	u.setActivity("Ready")
	if loaded.refresh {
		diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "startup"), slog.String("scope_type", "quick"))
		u.startFiles(model.QuickScope, "", false)
	} else if u.tabs.CurrentIndex() == tabDocker {
		u.tabChanged()
	}
}

// showDrives builds one card per fixed drive the first time drives are
// known, then updates their free space.
func (u *gui) showDrives(disks []model.DriveState) {
	if len(u.drives) == 0 && len(disks) > 0 {
		u.buildDriveCards(disks)
	}
	for index, label := range u.driveLabels {
		position := slicesIndex(disks, u.drives[index])
		if position < 0 || disks[position].Total <= 0 {
			_ = label.SetText("Free space unavailable")
			u.driveBars[index].SetValue(0)
			continue
		}
		drive := disks[position]
		_ = label.SetText(units.Size(drive.Free) + " free of " + units.Size(drive.Total))
		u.driveBars[index].SetValue(int(float64(drive.Total-drive.Free) * 1000 / float64(drive.Total)))
	}
}

func slicesIndex(disks []model.DriveState, root string) int {
	for index, disk := range disks {
		if disk.Root == root {
			return index
		}
	}
	return -1
}

func (u *gui) buildDriveCards(disks []model.DriveState) {
	builder := d.NewBuilder(u.driveStrip)
	for _, disk := range disks {
		var label *walk.Label
		var bar *walk.ProgressBar
		card := d.GroupBox{
			Title: disk.Root[:2], StretchFactor: 1,
			Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 6, Right: 10, Bottom: 6}, Spacing: 5},
			Children: []d.Widget{
				d.Label{AssignTo: &label, Text: "Reading free space…", NoPrefix: true},
				d.ProgressBar{AssignTo: &bar, MaxValue: 1000, MinSize: d.Size{Height: 8}, MaxSize: d.Size{Height: 8}},
			},
		}
		if err := card.Create(builder); err != nil {
			diag.Log(u.ctx, slog.LevelWarn, "gui.drive_card_failed", slog.String("error_kind", diag.ErrorKind(err)))
			continue
		}
		u.drives = append(u.drives, disk.Root)
		u.driveLabels = append(u.driveLabels, label)
		u.driveBars = append(u.driveBars, bar)
	}
	u.setFileScope(combo(u.fileScope, model.QuickScope))
}

func combo(box *walk.ComboBox, fallback string) string {
	if box != nil && box.CurrentIndex() >= 0 {
		return box.Text()
	}
	return fallback
}

func timeText(milliseconds int64) string {
	if milliseconds <= 0 {
		return "—"
	}
	return time.UnixMilli(milliseconds).Format("2006-01-02 15:04:05")
}

func fileDate(milliseconds int64) string {
	if milliseconds <= 0 {
		return "—"
	}
	return time.UnixMilli(milliseconds).Format("2006-01-02")
}

func dailyCaptureDue(s model.Snapshot, now time.Time) bool {
	return s.ID != 0 && now.Sub(time.UnixMilli(s.Captured)) >= dailyInterval
}

func (u *gui) scheduleCaptures() {
	ticker := time.NewTicker(scheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case now := <-ticker.C:
			u.dispatch(func() { u.captureDue(now) })
		}
	}
}

// captureDue refreshes the selected Quick review or the open Docker tab once
// a day, after checking that no other process captured it meanwhile.
func (u *gui) captureDue(now time.Time) {
	backend := u.backend
	if backend == nil || u.ctx.Err() != nil || u.active != nil || u.dueLoad != nil {
		return
	}
	dockerDue := u.tabs.CurrentIndex() == tabDocker && dailyCaptureDue(u.docker, now)
	filesDue := combo(u.fileScope, "") == model.QuickScope && u.files.Label == model.QuickScope && dailyCaptureDue(u.files, now)
	if !dockerDue && !filesDue {
		return
	}
	dockerID, filesID := u.docker.Scope, u.files.Scope
	type scheduled struct{ docker, files model.Snapshot }
	readForGUI(u, &u.dueLoad, "gui.scheduled_lookup", func(ctx context.Context) (scheduled, error) {
		var saved scheduled
		var dockerErr, filesErr error
		if dockerDue {
			saved.docker, dockerErr = latestSnapshot(ctx, backend, dockerID)
		}
		if filesDue {
			saved.files, filesErr = latestSnapshot(ctx, backend, filesID)
		}
		return saved, errors.Join(dockerErr, filesErr)
	}, func(saved scheduled, _ error) {
		if u.active != nil {
			return
		}
		if dockerDue && u.tabs.CurrentIndex() == tabDocker && u.docker.Scope == dockerID &&
			u.refreshDue(&u.docker, saved.docker, now, u.renderDocker) {
			diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "daily"), slog.String("scope_type", "docker"))
			u.startDocker()
			return
		}
		if filesDue && combo(u.fileScope, "") == model.QuickScope && u.files.Scope == filesID &&
			u.refreshDue(&u.files, saved.files, now, u.renderFiles) {
			diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "daily"), slog.String("scope_type", "quick"))
			u.startFiles(model.QuickScope, "", false)
		}
	})
}

func latestSnapshot(ctx context.Context, backend service.Backend, scope string) (model.Snapshot, error) {
	latest, err := backend.Latest(ctx, service.LatestQuery{Scope: scope})
	return latest.Snapshot, err
}

func (u *gui) refreshDue(saved *model.Snapshot, latest model.Snapshot, now time.Time, render func()) bool {
	if latest.ID > saved.ID {
		*saved = latest
		render()
		u.updateScopes()
	}
	return dailyCaptureDue(*saved, now)
}

func (u *gui) openSelection(pane *rankedPane) {
	row, ok := pane.selected()
	if !ok || row.Item.Path == "" {
		return
	}
	path := row.Item.Path
	if !row.Item.Directory {
		path = filepath.Dir(path)
	}
	u.openFolder(path, "gui.open_selection", "Folder unavailable", true)
}

// closestFolder returns the nearest existing folder at or above path.
func closestFolder(path string) (string, error) {
	for {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			return path, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("%s is no longer available", path)
		}
		path = parent
	}
}

func exploreFolder(path string) error {
	folder, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// S_FALSE means COM was already initialized on this thread; it still needs uninitializing.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return err
	}
	defer windows.CoUninitialize()
	verb, _ := windows.UTF16PtrFromString("explore")
	return windows.ShellExecute(0, verb, folder, nil, nil, swShowNormal)
}

func (u *gui) openFolder(path, event, title string, findParent bool) {
	ctx := diag.Operation(u.ctx)
	go func() {
		defer diag.RecoverPanic(ctx, event)
		started := time.Now()
		var err error
		if findParent {
			path, err = closestFolder(path)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = exploreFolder(path)
		}
		diag.Duration(ctx, event, started, err)
		if err != nil {
			u.dispatch(func() { walk.MsgBox(u.window, title, err.Error(), walk.MsgBoxIconWarning) })
		}
	}()
}

func (u *gui) tabChanged() {
	if u.tabs == nil || u.window == nil {
		return
	}
	switch u.tabs.CurrentIndex() {
	case tabDocker:
		if u.backend != nil && !u.dockerOpened && u.active == nil {
			u.dockerOpened = true
			if u.docker.ID == 0 || time.Since(time.UnixMilli(u.docker.Captured)) >= dockerFreshness {
				diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "docker_tab"), slog.String("scope_type", "docker"))
				u.startDocker()
			}
		}
	case tabChanges:
		u.updateScopes()
	}
	u.updateActions()
}

func (u *gui) fileCaptureActive() bool {
	return u.active != nil && u.active.scope != model.DockerScope
}

func (u *gui) updateActions() {
	if u.tabs == nil || u.cancelButton == nil || u.changeCapture == nil || u.captureMenu == nil {
		return
	}
	idle := u.active == nil
	ready := u.backend != nil
	fileActive := u.fileCaptureActive()
	u.scanButton.SetEnabled(ready && idle)
	u.dockerButton.SetEnabled(ready && idle)
	u.changeCapture.SetEnabled(ready && idle)
	u.fileScope.SetEnabled(ready && !fileActive)
	u.browse.SetEnabled(ready && idle)
	u.backButton.SetEnabled(!fileActive && len(u.back) != 0)
	if u.filePane.inspectBlocked != !idle {
		u.filePane.inspectBlocked = !idle
		u.filePane.showDetail()
	}
	canCancel := !idle && !u.active.stopping
	u.cancelButton.SetVisible(!idle)
	u.cancelButton.SetEnabled(canCancel)
	u.activityBar.SetVisible(!idle)
	_ = u.cancelMenu.SetEnabled(canCancel)
	_ = u.captureMenu.SetEnabled(ready && idle && u.tabs.CurrentIndex() != tabMCP)
	_ = u.reloadMenu.SetEnabled(!u.opening)
	search := u.activeSearch()
	_ = u.findMenu.SetEnabled(search != nil && search.Enabled())
	_ = u.changeCapture.SetText(u.changeCaptureText())
}

func (u *gui) setActivity(message string) {
	_ = u.activity.SetText(message)
	_ = u.activity.SetToolTipText(message)
}

func (u *gui) beginCapture(scope, message string) context.Context {
	ctx, cancel := context.WithCancel(diag.Operation(u.ctx))
	u.active = &captureTask{scope: scope, ctx: ctx, cancel: cancel, started: time.Now()}
	u.setActivity(message)
	u.updateActions()
	_ = u.activityBar.SetMarqueeMode(true)
	if u.tabs.CurrentIndex() == tabChanges && u.comparison.Baseline.ID == 0 {
		u.renderChanges()
	}
	return ctx
}

func (u *gui) cancelActive() {
	if u.active == nil || u.active.stopping {
		return
	}
	u.active.stopping = true
	diag.Log(u.active.ctx, slog.LevelInfo, "gui.cancel_requested")
	u.active.cancel()
	u.setActivity("Canceling " + u.active.scope + "…")
	_ = u.activityBar.SetMarqueeMode(false)
	u.updateActions()
}

func (u *gui) applyCapture(task *captureTask, err error, apply func()) {
	queued := time.Now()
	u.dispatch(func() {
		applying := time.Now()
		u.finishCapture(task, err)
		apply()
		diag.Duration(task.ctx, "gui.capture", task.started, err, slog.String("scope_type", model.ScopeType(task.scope)),
			slog.Float64("ui_queue_ms", diag.Milliseconds(applying.Sub(queued))),
			slog.Float64("apply_ms", diag.Milliseconds(time.Since(applying))))
	})
}

func (u *gui) finishCapture(task *captureTask, err error) {
	task.cancel()
	u.active = nil
	_ = u.activityBar.SetMarqueeMode(false)
	u.updateActions()
	switch {
	case err == nil:
		u.setActivity(task.scope + " capture saved.")
	case errors.Is(err, context.Canceled):
		u.setActivity("Capture canceled. Saved history is still available.")
	default:
		u.setActivity("Capture failed. " + err.Error())
	}
	if u.tabs.CurrentIndex() == tabChanges && u.comparison.Baseline.ID == 0 {
		u.renderChanges()
	}
}

func (u *gui) captureCurrent() {
	switch u.tabs.CurrentIndex() {
	case tabChanges:
		u.captureChanges()
	case tabFiles:
		u.scanClick()
	case tabDocker:
		u.startDocker()
	}
}

func (u *gui) activeSearch() *walk.LineEdit {
	switch u.tabs.CurrentIndex() {
	case tabChanges:
		return u.changeFilter
	case tabFiles:
		return u.fileFilter
	case tabDocker:
		return u.dockerFilter
	}
	return nil
}

func (u *gui) focusSearch() {
	if search := u.activeSearch(); search != nil && search.Enabled() {
		_ = search.SetFocus()
		search.SetTextSelection(0, -1)
	}
}

// updateScopes reloads the saved scopes, or reconnects when the backend
// could not be opened.
func (u *gui) updateScopes() {
	backend := u.backend
	if u.changeScope == nil {
		return
	}
	if backend == nil {
		u.openBackend()
		return
	}
	readForGUI(u, &u.scopeLoad, "gui.history", backend.Scopes, func(list []model.Snapshot, err error) {
		if err != nil {
			_ = u.changeStatus.SetText("Could not reload history. " + err.Error())
			return
		}
		u.applyScopes(list)
	})
}
