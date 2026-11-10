// Package ui is the Space Review desktop app, drawn with Gio. It depends only
// on the service.Backend contract, so it runs the same against the in-process
// backend or a remote one reached over the gRPC API.
package ui

import (
	"context"
	"errors"
	"image"
	"log/slog"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/io/semantic"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

const (
	viewChanges = iota
	viewFiles
	viewDocker
	viewMCP
	viewCount
)

var (
	viewNames        = [viewCount]string{"Changes", "Files", "Docker", "MCP"}
	viewIcons        = [viewCount]icon{iconActivity, iconFolder, iconBoxes, iconPlug}
	viewDescriptions = [viewCount]string{
		"Compare two saved storage captures.",
		"See what uses disk space.",
		"Review storage used by the Docker engine.",
		"Use the same saved history in an assistant.",
	}
)

const (
	selectPrompt     = "Select an item to see its details."
	slowRender       = 32 * time.Millisecond
	scheduleInterval = 15 * time.Minute
	dockerFreshness  = 15 * time.Minute
	dailyInterval    = 24 * time.Hour
	historyDepth     = 8
)

// Options configure the desktop app.
type Options struct {
	// Open connects to the backend. It runs on a worker so the window can
	// appear first; the app closes the backend when it exits.
	Open func(context.Context) (service.Backend, error)
	// Location describes where history is kept, shown in the MCP view.
	Location string
	// MCPCommand and MCPArgs start an MCP server that uses the same history.
	MCPCommand string
	MCPArgs    []string
	// DiagnosticsDir is opened from the sidebar; empty when logging is off.
	DiagnosticsDir string
}

type captureTask struct {
	scope    string
	ctx      context.Context
	started  time.Time
	cancel   context.CancelFunc
	stopping bool
	progress service.Progress
}

type guiRead struct{ cancel context.CancelFunc }

func cancelGUIRead(slot **guiRead) {
	if current := *slot; current != nil {
		*slot = nil
		current.cancel()
	}
}

// statusTone colors the status dot.
type statusTone int

const (
	toneReady statusTone = iota
	toneBusy
	toneProblem
)

type ui struct {
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	win     *app.Window
	hwnd    uintptr
	th      *theme
	mu      sync.Mutex
	queue   []func()
	closed  bool
	backend service.Backend
	opening bool
	opened  <-chan service.Backend

	view             int
	nav              [viewCount]widget.Clickable
	wideResults      bool
	inspectorHidden  bool
	inspectorButton  widget.Clickable
	details          *detailsDialog
	returnFocus      *widget.Clickable
	summaryButton    widget.Clickable
	captureButton    widget.Clickable
	captureWasCancel bool
	more             selectBox
	reloadButton     widget.Clickable
	diagButton       widget.Clickable
	sidebarList      widget.List
	mcpList          widget.List

	drives     []model.DriveState
	driveRoots []string
	status     string
	tone       statusTone
	active     *captureTask
	dueLoad    *guiRead

	assistant    *assistantDialog
	export       *exportDialog
	exporting    bool
	exclusions   *exclusionsDialog
	scheduled    *scheduleDialog
	duplicates   *duplicatesDialog
	recycle      *recycleDialog
	coverageInfo *coverageDialog
	alert        *alertDialog
	clipboard    *string
	// themeFixed stops following the system theme, for QA builds.
	themeFixed bool

	changesTab
	filesTab
	dockerTab
	mcpTab
}

// post runs f on the UI goroutine before the next frame.
func (u *ui) post(f func()) {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.queue = append(u.queue, f)
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *ui) drain() {
	u.mu.Lock()
	queue := u.queue
	u.queue = nil
	u.mu.Unlock()
	for _, f := range queue {
		if u.ctx.Err() == nil {
			f()
		}
	}
}

func (u *ui) stop() {
	u.mu.Lock()
	u.closed = true
	u.cancel()
	u.mu.Unlock()
}

// readForGUI runs a read on a worker and applies its result on the UI
// goroutine. Only the UI goroutine owns the slots, and a newer read in the
// same slot discards the result of an older one.
func readForGUI[T any](u *ui, slot **guiRead, event string, read func(context.Context) (T, error), apply func(T, error)) {
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
		u.post(func() {
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

// Run shows the window until the user closes it.
func Run(parent context.Context, opts Options) error {
	started := time.Now()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	u := &ui{opts: opts, ctx: ctx, cancel: cancel, th: &theme{palette: systemPalette(), shaper: newShaper()}}
	u.win = new(app.Window)
	u.win.Option(app.Title("Space Review"), app.Size(1240, 800), app.MinSize(560, 460))
	u.initViews()
	u.setActivity("Opening saved history…", toneBusy)
	u.openBackend()
	go func() {
		defer diag.RecoverPanic(ctx, "gui.scheduler")
		u.scheduleCaptures()
	}()
	go func() {
		defer diag.RecoverPanic(ctx, "gui.theme")
		u.watchTheme()
	}()
	var ops op.Ops
	var accessibility *accessWindow
	first := true
	focused := false
	for {
		switch e := u.win.Event().(type) {
		case app.ConfigEvent:
			if e.Config.Focused && !focused {
				u.checkFilePaths()
			}
			focused = e.Config.Focused
		case app.Win32ViewEvent:
			if e.Valid() {
				u.hwnd = e.HWND
				tintFrame(u.hwnd, u.th.palette)
				u.win.Run(func() { accessibility = attachAccessibility(u, e.HWND) })
				if accessibility != nil {
					u.th.access = &accessibility.frame
				}
			}
		case app.DestroyEvent:
			diag.Log(ctx, slog.LevelInfo, "gui.close", slog.Bool("capture_active", u.active != nil))
			u.stop()
			if backend := <-u.opened; backend != nil {
				_ = backend.Close()
			}
			return e.Err
		case app.FrameEvent:
			u.drain()
			gtx := app.NewContext(&ops, e)
			if accessibility != nil {
				accessibility.frame.begin(gtx)
			}
			u.layout(gtx)
			if accessibility != nil {
				changed, focus := accessibility.frame.snapshot(gtx.Ops, e.Size)
				accessibility.notify(changed, focus)
			}
			e.Frame(gtx.Ops)
			if first {
				first = false
				diag.Duration(ctx, "gui.window_create", started, nil)
			}
		}
	}
}

// watchTheme follows the Windows light, dark and high contrast settings.
func (u *ui) watchTheme() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-ticker.C:
			p := systemPalette()
			u.post(func() {
				if p != u.th.palette && !u.themeFixed {
					u.th.palette = p
					tintFrame(u.hwnd, p)
				}
			})
		}
	}
}

// openBackend connects on a worker so the window shows while history loads.
func (u *ui) openBackend() {
	if u.backend != nil || u.opening {
		return
	}
	u.opening = true
	u.setActivity("Opening saved history…", toneBusy)
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

func (u *ui) loadStartup(opened chan<- service.Backend) {
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
	u.post(func() { u.applyStartup(loaded, started, err) })
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

func (u *ui) applyStartup(loaded startup, started time.Time, err error) {
	u.opening = false
	u.backend, u.files, u.docker = loaded.backend, loaded.files, loaded.docker
	u.showDrives(loaded.disks)
	u.renderFiles()
	u.renderDocker()
	u.applyScopes(loaded.scopes)
	diag.Duration(u.ctx, "gui.startup", started, err, slog.Bool("saved_files", loaded.files.ID != 0),
		slog.Bool("saved_docker", loaded.docker.ID != 0), slog.Int("fixed_drives", len(loaded.disks)))
	if err != nil {
		u.setActivity("Could not load saved history. Use Reload history in the sidebar to retry. "+err.Error(), toneProblem)
		return
	}
	u.setActivity("Ready", toneReady)
	if loaded.refresh {
		diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "startup"), slog.String("scope_type", "quick"))
		u.startFiles(model.QuickScope, "", false)
	} else if u.view == viewDocker {
		u.viewChanged()
	}
}

func (u *ui) showDrives(disks []model.DriveState) {
	if len(disks) == 0 {
		return
	}
	u.drives = disks
	if len(u.driveRoots) == 0 {
		for _, disk := range disks {
			u.driveRoots = append(u.driveRoots, disk.Root)
		}
		u.setFileScope(u.fileScopeValue())
	}
}

func (u *ui) setActivity(message string, tone statusTone) {
	u.status, u.tone = message, tone
}

func dailyCaptureDue(s model.Snapshot, now time.Time) bool {
	return s.ID != 0 && now.Sub(time.UnixMilli(s.Captured)) >= dailyInterval
}

func (u *ui) scheduleCaptures() {
	ticker := time.NewTicker(scheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case now := <-ticker.C:
			u.post(func() { u.captureDue(now) })
		}
	}
}

// captureDue refreshes the selected Quick review or the open Docker view once
// a day, after checking that no other process captured it meanwhile.
func (u *ui) captureDue(now time.Time) {
	backend := u.backend
	if backend == nil || u.ctx.Err() != nil || u.active != nil || u.dueLoad != nil || u.recycle != nil {
		return
	}
	dockerDue := u.view == viewDocker && dailyCaptureDue(u.docker, now)
	filesDue := u.fileScopeValue() == model.QuickScope && u.files.Label == model.QuickScope && dailyCaptureDue(u.files, now)
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
		if u.active != nil || u.recycle != nil {
			return
		}
		if dockerDue && u.view == viewDocker && u.docker.Scope == dockerID &&
			u.refreshDue(&u.docker, saved.docker, now, u.renderDocker) {
			diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "daily"), slog.String("scope_type", "docker"))
			u.startDocker()
			return
		}
		if filesDue && u.fileScopeValue() == model.QuickScope && u.files.Scope == filesID &&
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

func (u *ui) refreshDue(saved *model.Snapshot, latest model.Snapshot, now time.Time, render func()) bool {
	if latest.ID > saved.ID {
		*saved = latest
		render()
		u.updateScopes()
	}
	return dailyCaptureDue(*saved, now)
}

func (u *ui) setView(index int) {
	if index == u.view || index < 0 || index >= viewCount {
		return
	}
	u.view = index
	u.more.open = false
	u.more.Changed()
	u.details = nil
	u.returnFocus = nil
	u.viewChanged()
}

func (u *ui) viewChanged() {
	if u.view != viewFiles {
		cancelGUIRead(&u.fileCheck)
	}
	switch u.view {
	case viewFiles:
		u.checkFilePaths()
	case viewDocker:
		if u.backend != nil && !u.dockerOpened && u.active == nil {
			u.dockerOpened = true
			if u.docker.ID == 0 || time.Since(time.UnixMilli(u.docker.Captured)) >= dockerFreshness {
				diag.Log(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "docker_tab"), slog.String("scope_type", "docker"))
				u.startDocker()
			}
		}
	case viewChanges:
		u.updateScopes()
	}
}

func (u *ui) fileCaptureActive() bool {
	return u.active != nil && u.active.scope != model.DockerScope
}

func (u *ui) beginCapture(scope, message string) context.Context {
	ctx, cancel := context.WithCancel(diag.Operation(u.ctx))
	u.active = &captureTask{scope: scope, ctx: ctx, cancel: cancel, started: time.Now()}
	u.setActivity(message, toneBusy)
	u.filePane.inspectBlocked = true
	if u.view == viewChanges && u.comparison.Baseline.ID == 0 {
		u.renderChanges()
	}
	return ctx
}

func (u *ui) cancelActive() {
	if u.active == nil || u.active.stopping {
		return
	}
	u.active.stopping = true
	diag.Log(u.active.ctx, slog.LevelInfo, "gui.cancel_requested")
	u.active.cancel()
	u.setActivity("Canceling "+u.active.scope+"…", toneBusy)
}

func (u *ui) captureProgress(task *captureTask) func(service.Progress) {
	return func(progress service.Progress) {
		u.post(func() {
			if u.active == task && task.ctx.Err() == nil {
				task.progress = progress
			}
		})
	}
}

func (u *ui) applyCapture(task *captureTask, err error, apply func()) {
	queued := time.Now()
	u.post(func() {
		applying := time.Now()
		u.finishCapture(task, err)
		apply()
		diag.Duration(task.ctx, "gui.capture", task.started, err, slog.String("scope_type", model.ScopeType(task.scope)),
			slog.Float64("ui_queue_ms", diag.Milliseconds(applying.Sub(queued))),
			slog.Float64("apply_ms", diag.Milliseconds(time.Since(applying))))
	})
}

func (u *ui) finishCapture(task *captureTask, err error) {
	task.cancel()
	u.active = nil
	u.filePane.inspectBlocked = false
	switch {
	case err == nil:
		u.setActivity(task.scope+" capture saved.", toneReady)
	case errors.Is(err, context.Canceled):
		u.setActivity("Capture canceled. Saved history is still available.", toneReady)
	default:
		u.setActivity("Capture failed. "+err.Error(), toneProblem)
	}
	if u.view == viewChanges && u.comparison.Baseline.ID == 0 {
		u.renderChanges()
	}
}

func (u *ui) captureCurrent() {
	if u.backend == nil || u.active != nil {
		return
	}
	switch u.view {
	case viewChanges:
		u.captureChanges()
	case viewFiles:
		u.scanClick()
	case viewDocker:
		u.startDocker()
	}
}

func (u *ui) captureText() string {
	switch u.view {
	case viewChanges:
		return u.changeCaptureText()
	case viewDocker:
		return "Refresh Docker"
	}
	return "Scan now"
}

func (u *ui) activeSearch() *searchField {
	switch u.view {
	case viewChanges:
		return &u.changeFilter
	case viewFiles:
		return &u.fileFilter
	case viewDocker:
		return &u.dockerFilter
	}
	return nil
}

func (u *ui) activePane() *rankedPane {
	switch u.view {
	case viewChanges:
		return &u.changePane
	case viewFiles:
		return &u.filePane
	case viewDocker:
		return &u.dockerPane
	}
	return nil
}

// updateScopes reloads the saved scopes, or reconnects when the backend
// could not be opened.
func (u *ui) updateScopes() {
	backend := u.backend
	if backend == nil {
		u.openBackend()
		return
	}
	readForGUI(u, &u.scopeLoad, "gui.history", backend.Scopes, func(list []model.Snapshot, err error) {
		if err != nil {
			u.changeStatus = "Could not reload history. " + err.Error()
			return
		}
		u.applyScopes(list)
	})
}

// globalKeys handles the window shortcuts.
func (u *ui) globalKeys(gtx layout.Context) {
	for {
		e, ok := gtx.Event(
			key.Filter{Name: "F5"},
			key.Filter{Name: "F", Required: key.ModShortcut},
			key.Filter{Name: "I", Required: key.ModShortcut},
			key.Filter{Name: key.NameEscape},
		)
		if !ok {
			break
		}
		k, ok := e.(key.Event)
		if !ok || k.State != key.Press {
			continue
		}
		modal := u.dialogActive()
		switch k.Name {
		case "F5":
			if !modal {
				u.captureCurrent()
			}
		case "F":
			if search := u.activeSearch(); search != nil && !modal {
				gtx.Execute(key.FocusCmd{Tag: &search.editor})
				search.editor.SetCaret(search.editor.Len(), 0)
			}
		case "I":
			if !modal && u.activePane() != nil {
				u.toggleDetails(false)
			} else if u.details != nil && u.details.summary == nil && !u.details.showMap && u.alert == nil {
				u.closeDetails(gtx)
			}
		case key.NameEscape:
			switch {
			case u.alert != nil:
				u.alert = nil
			case u.recycle != nil:
				u.closeRecycle()
			case u.export != nil:
				u.export = nil
			case u.exclusions != nil:
				u.exclusions = nil
			case u.scheduled != nil:
				u.closeSchedule()
			case u.duplicates != nil:
				u.closeDuplicates()
			case u.coverageInfo != nil:
				u.coverageInfo = nil
			case u.fileFilters != nil:
				if !u.fileFilters.closePicker() {
					u.closeFileFilters(gtx)
				}
			case u.details != nil:
				u.closeDetails(gtx)
			case u.assistant != nil:
				u.assistant = nil
			case u.more.open:
				u.more.open = false
			}
		}
	}
}

// qaFrame is set only in QA builds, which can put the window in a given
// state for layout checks.
var qaFrame func(*ui, layout.Context)

func (u *ui) dialogActive() bool {
	return u.assistant != nil || u.export != nil || u.exclusions != nil || u.scheduled != nil || u.duplicates != nil ||
		u.coverageInfo != nil || u.fileFilters != nil || u.details != nil || u.recycle != nil || u.alert != nil
}

func (u *ui) layout(gtx layout.Context) {
	width := gtx.Metric.PxToDp(gtx.Constraints.Max.X)
	compact := width < 960
	workspace := width - sidebarWidth(compact)
	u.wideResults = workspace-2*workspaceInset(workspace) >= 680
	if qaFrame != nil {
		qaFrame(u, gtx)
	}
	u.flushClipboard(gtx)
	u.globalKeys(gtx)
	t := u.th
	paint.Fill(gtx.Ops, t.surface)
	ugtx := gtx
	modal := u.dialogActive()
	if !modal && u.returnFocus != nil {
		gtx.Execute(key.FocusCmd{Tag: u.returnFocus})
		u.returnFocus = nil
	}
	var underlay clip.Stack
	if modal {
		ugtx = gtx.Disabled()
		underlay = clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
		semantic.DescriptionOp(accessHidden).Add(gtx.Ops)
	}
	layout.Flex{}.Layout(ugtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.sidebar(gtx, compact) }),
		layout.Flexed(1, u.workspace),
	)
	if modal {
		underlay.Pop()
	}
	if u.assistant != nil {
		u.assistant.layout(gtx, u)
	}
	if u.export != nil {
		u.export.layout(gtx, u)
	}
	if u.exclusions != nil {
		u.exclusions.layout(gtx, u)
	}
	if u.scheduled != nil {
		u.scheduled.layout(gtx, u)
	}
	if u.duplicates != nil {
		u.duplicates.layout(gtx, u)
	}
	if u.fileFilters != nil {
		u.fileFilters.layout(gtx, u)
	}
	if u.details != nil {
		u.details.layout(gtx, u)
	}
	if u.coverageInfo != nil {
		u.coverageInfo.layout(gtx, u)
	}
	if u.recycle != nil {
		u.recycle.layout(gtx, u)
	}
	if u.alert != nil {
		u.alert.layout(gtx, u)
	}
	if u.active != nil || (u.duplicates != nil && u.duplicates.request != nil) {
		// Animate the progress strip while a capture runs.
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(40 * time.Millisecond)})
	}
}

func (u *ui) sidebar(gtx layout.Context, compact bool) layout.Dimensions {
	t := u.th
	width := gtx.Dp(sidebarWidth(compact))
	size := image.Pt(width, gtx.Constraints.Max.Y)
	gtx.Constraints = layout.Exact(size)
	paint.FillShape(gtx.Ops, t.chrome, clipRect(size))
	for i := range u.nav {
		if u.nav[i].Clicked(gtx) {
			u.setView(i)
		}
	}
	if u.reloadButton.Clicked(gtx) {
		u.updateScopes()
	}
	if u.diagButton.Clicked(gtx) && u.opts.DiagnosticsDir != "" {
		u.openFolder(u.opts.DiagnosticsDir, "gui.open_diagnostics", "Diagnostics folder unavailable", false)
	}
	pad := layout.Inset{Top: 14, Bottom: 12, Left: 12, Right: 12}
	if compact {
		pad.Left, pad.Right = 10, 10
	}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = gtx.Constraints.Max
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.brand(gtx, compact) }),
			layout.Rigid(space(0, 18)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				var children []layout.FlexChild
				for i := range u.nav {
					if i > 0 {
						children = append(children, layout.Rigid(space(0, 3)))
					}
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.navItem(gtx, i, compact)
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			}),
			layout.Rigid(space(0, 26)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return scrollColumn(gtx, &u.sidebarList, func(gtx layout.Context) layout.Dimensions {
					return u.driveList(gtx, compact)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.sidebarFooter(gtx, compact) }),
		)
	})
}

func (u *ui) brand(gtx layout.Context, compact bool) layout.Dimensions {
	t := u.th
	mark := func(gtx layout.Context) layout.Dimensions {
		size := image.Pt(gtx.Dp(30), gtx.Dp(30))
		fill(gtx, t.accent, size, 8)
		stack := op.Offset(image.Pt(gtx.Dp(6), gtx.Dp(6))).Push(gtx.Ops)
		drawIcon(gtx, iconDrive, 18, t.onAccent)
		stack.Pop()
		return layout.Dimensions{Size: size}
	}
	if compact {
		return layout.Inset{Left: 3}.Layout(gtx, mark)
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(mark),
		layout.Rigid(space(10, 0)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.strong(16, t.ink), "Space Review")
		}),
	)
}

func (u *ui) navItem(gtx layout.Context, index int, compact bool) layout.Dimensions {
	t := u.th
	click := &u.nav[index]
	selected := u.view == index
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(38))
		defer t.exposeButton(gtx, size, click, viewNames[index]).Pop()
		switch {
		case selected:
			fill(gtx, t.selected, size, 8)
		case click.Hovered():
			fill(gtx, t.well, size, 8)
		}
		fg := t.ink
		if selected {
			fg = t.blue
			if t.contrast {
				fg = t.onAccent
			}
		}
		x := gtx.Dp(11)
		if compact {
			x = (size.X - gtx.Dp(18)) / 2
		}
		stack := op.Offset(image.Pt(x, (size.Y-gtx.Dp(18))/2)).Push(gtx.Ops)
		drawIcon(gtx, viewIcons[index], 18, fg)
		stack.Pop()
		if !compact {
			lgtx := gtx
			lgtx.Constraints = layout.Constraints{Max: image.Pt(size.X-gtx.Dp(44), size.Y)}
			s := t.body(fg)
			if selected {
				s = t.strong(14, fg)
			}
			rec := op.Record(gtx.Ops)
			d := t.label(lgtx, s, viewNames[index])
			call := rec.Stop()
			stack = op.Offset(image.Pt(gtx.Dp(40), (size.Y-d.Size.Y)/2)).Push(gtx.Ops)
			call.Add(gtx.Ops)
			stack.Pop()
		}
		if gtx.Focused(click) {
			t.focusRing(gtx, size, 8)
		}
		return layout.Dimensions{Size: size}
	})
}

func (u *ui) driveList(gtx layout.Context, compact bool) layout.Dimensions {
	t := u.th
	children := []layout.FlexChild{}
	if !compact {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return t.label(gtx, t.strong(12, t.quiet), "Live drive space")
				})
			}),
			layout.Rigid(space(0, 10)))
	}
	if len(u.drives) == 0 {
		if !compact {
			msg := "Reading drives…"
			if !u.opening {
				msg = "No drive readings"
			}
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return t.label(gtx, t.small(t.quiet), msg)
				})
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	}
	for i, disk := range u.drives {
		if i > 0 {
			children = append(children, layout.Rigid(space(0, 14)))
		}
		used := float32(0)
		if disk.Total > 0 {
			used = float32(disk.Total-disk.Free) / float32(disk.Total)
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 4, Right: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return t.label(gtx, t.strong(13, t.ink), driveName(disk.Root))
					}),
					layout.Rigid(space(0, 6)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						barColor := t.blue
						if used > 0.9 && !t.contrast {
							barColor = t.grow
						}
						return t.meter(gtx, used, barColor, 5)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if compact {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return t.label(gtx, t.small(t.quiet).wrapped(2), driveCaption(disk))
						})
					}),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (u *ui) sidebarFooter(gtx layout.Context, compact bool) layout.Dimensions {
	t := u.th
	gtx.Constraints.Min.X = 0
	if compact {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &u.reloadButton, buttonStyle{kind: quiet, icon: iconRefresh, compact: true,
					accessibleName: "Reload history", disabled: u.opening})
			}),
		)
	}
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.button(gtx, &u.reloadButton, buttonStyle{kind: quiet, icon: iconRefresh, label: "Reload history", compact: true, disabled: u.opening})
		}),
	}
	if u.opts.DiagnosticsDir != "" {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.button(gtx, &u.diagButton, buttonStyle{kind: quiet, icon: iconDiagnos, label: "Diagnostics", compact: true})
		}))
	}
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: 8, Left: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.small(t.quiet), "Read-only scans")
		})
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func sidebarWidth(compact bool) unit.Dp {
	if compact {
		return 64
	}
	return 220
}

func workspaceInset(width unit.Dp) unit.Dp {
	if width < 640 {
		return 14
	}
	return 26
}

// workspace is the main area: heading, the current view and the status strip.
func (u *ui) workspace(gtx layout.Context) layout.Dimensions {
	t := u.th
	if u.view == viewChanges && u.viewFirstScan.Clicked(gtx) && u.canOpenFirstScan() {
		u.openFirstScan()
		gtx.Execute(op.InvalidateCmd{})
	}
	if u.captureButton.Clicked(gtx) {
		if !u.captureWasCancel && u.active == nil {
			u.captureCurrent()
		} else if u.captureWasCancel && u.active != nil {
			u.cancelActive()
		}
	}
	side := workspaceInset(gtx.Metric.PxToDp(gtx.Constraints.Max.X))
	return layout.Inset{Left: side, Right: side, Top: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = gtx.Constraints.Max
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.heading),
			layout.Rigid(space(0, 14)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				switch u.view {
				case viewChanges:
					return u.changesView(gtx)
				case viewFiles:
					return u.filesView(gtx)
				case viewDocker:
					return u.dockerView(gtx)
				default:
					return u.mcpView(gtx)
				}
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.statusStrip(gtx, t) }),
		)
	})
}

func (u *ui) heading(gtx layout.Context) layout.Dimensions {
	t := u.th
	title := func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(26, t.ink), viewNames[u.view])
			}),
			layout.Rigid(space(0, 2)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.quiet).wrapped(2), viewDescriptions[u.view])
			}),
		)
	}
	if u.view == viewMCP {
		if u.active != nil {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, title), layout.Rigid(space(16, 0)), layout.Rigid(u.captureControl))
		}
		return title(gtx)
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, title),
		layout.Rigid(space(16, 0)),
		layout.Rigid(u.moreActions),
		layout.Rigid(space(8, 0)),
		layout.Rigid(u.captureControl),
	)
}

func (u *ui) moreActions(gtx layout.Context) layout.Dimensions {
	type action struct {
		label    string
		disabled bool
		run      func()
	}
	actions := []action{
		{"Ask assistant", false, u.askAssistant},
		{"Export current view…", u.reportSource().ID == 0 || u.exporting, u.openExport},
		{"Schedule scans…", u.backend == nil || u.opts.MCPCommand == "", u.openSchedule},
	}
	if u.view == viewFiles {
		actions = append(actions,
			action{"Find duplicates…", !u.files.Coverage.Inventory || u.active != nil, u.openDuplicates},
			action{"Scan coverage", u.files.ID == 0, u.showScanCoverage},
			action{"Scan exclusions…", u.backend == nil || u.active != nil, u.openExclusions},
		)
	}
	u.more.command = "More"
	names := make([]string, len(actions))
	u.more.unavailable = make([]bool, len(actions))
	for i, item := range actions {
		names[i], u.more.unavailable[i] = item.label, item.disabled
	}
	u.more.setOptions(names, u.more.index)
	if u.more.Changed() && u.more.index >= 0 {
		item := actions[u.more.index]
		if !item.disabled {
			item.run()
			if u.dialogActive() {
				u.returnFocus = &u.more.button
			}
			gtx.Execute(op.InvalidateCmd{})
		}
	}
	u.more.index = -1
	return u.more.Layout(gtx, u.th)
}

func (u *ui) captureControl(gtx layout.Context) layout.Dimensions {
	t := u.th
	label := u.captureText()
	ic := iconScan
	if label == "Refresh Docker" {
		ic = iconRefresh
	}
	style := buttonStyle{kind: primary, icon: ic, label: label, shortcut: "F5", disabled: u.backend == nil}
	u.captureWasCancel = u.active != nil
	if u.captureWasCancel {
		style = buttonStyle{icon: iconClose, label: "Cancel scan", disabled: u.active.stopping}
	}
	width := max(t.measure(gtx, t.strong(14, t.ink), label), t.measure(gtx, t.strong(14, t.ink), "Cancel scan"))
	gtx.Constraints.Min.X = min(gtx.Constraints.Max.X, width+gtx.Dp(49))
	return t.button(gtx, &u.captureButton, style)
}

func (u *ui) statusStrip(gtx layout.Context, t *theme) layout.Dimensions {
	return layout.Inset{Top: 8, Bottom: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		left := func(gtx layout.Context) layout.Dimensions {
			dot := t.free
			switch u.tone {
			case toneBusy:
				dot = t.accent
			case toneProblem:
				dot = t.grow
			}
			if u.active != nil {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.progress(gtx, t) }),
					layout.Rigid(space(8, 0)),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return t.label(gtx, t.small(t.ink), u.statusText())
					}),
				)
			}
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					size := image.Pt(gtx.Dp(7), gtx.Dp(7))
					fill(gtx, dot, size, 4)
					return layout.Dimensions{Size: size}
				}),
				layout.Rigid(space(8, 0)),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return t.label(gtx, t.small(t.quiet), u.status)
				}),
			)
		}
		right := func(gtx layout.Context) layout.Dimensions {
			txt := u.statusDates()
			if txt == "" {
				return layout.Dimensions{}
			}
			return t.label(gtx, t.small(t.quiet), txt)
		}
		h := gtx.Dp(30)
		gtx.Constraints.Min.Y = h
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, left),
			layout.Rigid(space(16, 0)),
			layout.Rigid(right),
		)
	})
}

func (u *ui) statusText() string {
	if u.active == nil || u.active.stopping {
		return u.status
	}
	text := u.status
	if u.active.progress.Saving {
		text = "Saving scan…"
	} else if u.active.progress.FilesChecked > 0 {
		text = "Scanning " + locationName(u.active.scope)
	}
	if checked := u.active.progress.FilesChecked; checked > 0 {
		text = thousands(checked) + " files checked  ·  " + text
	}
	return text
}

func (u *ui) statusDates() string {
	switch u.view {
	case viewFiles:
		if u.files.ID != 0 {
			return "Saved " + timeText(u.files.Captured)
		}
	case viewDocker:
		if u.docker.ID != 0 {
			return "Saved " + timeText(u.docker.Captured)
		}
	}
	return ""
}

// progress is an indeterminate progress strip.
func (u *ui) progress(gtx layout.Context, t *theme) layout.Dimensions {
	w, h := min(gtx.Dp(64), gtx.Constraints.Max.X), gtx.Dp(4)
	paint.FillShape(gtx.Ops, t.track, clipRRect(image.Pt(w, h), h/2, gtx))
	phase := float32(gtx.Now.UnixMilli()%1400) / 1400
	segment := w / 3
	x := int(phase*float32(w+segment)) - segment
	start, end := max(0, x), min(w, x+segment)
	if end > start {
		stack := op.Offset(image.Pt(start, 0)).Push(gtx.Ops)
		paint.FillShape(gtx.Ops, t.accent, clipRRect(image.Pt(end-start, h), h/2, gtx))
		stack.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, h)}
}

func timeText(milliseconds int64) string {
	if milliseconds <= 0 {
		return "—"
	}
	return time.UnixMilli(milliseconds).Format("2 Jan 2006 15:04")
}

func fileDate(milliseconds int64) string {
	if milliseconds <= 0 {
		return "—"
	}
	return time.UnixMilli(milliseconds).Format("2006-01-02")
}
