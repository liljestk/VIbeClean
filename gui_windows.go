package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"
)

var (
	periods     = []string{"Previous scan", "Last day", "Last week", "Last month", "Last 90 days"}
	periodDays  = []int{0, 1, 7, 30, 90}
	cutoffs     = []string{"128 MiB", "256 MiB", "512 MiB", "1 GiB", "5 GiB"}
	cutoffBytes = []int64{128 * mib, minChange, 512 * mib, 1 << 30, 5 << 30}
)

const (
	tabChanges = iota
	tabFiles
	tabDocker
	tabMCP
)

type displayRow struct {
	Item   record
	Cells  [4]string
	Rank   int64
	Before int64
	After  int64
	Detail string
}

type tableModel struct {
	walk.TableModelBase
	walk.SorterBase
	items []displayRow
	rows  []int
}

func (m *tableModel) RowCount() int { return len(m.rows) }

func (m *tableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) || col < 0 || col >= len(displayRow{}.Cells) {
		return ""
	}
	return m.items[m.rows[row]].Cells[col]
}

func (m *tableModel) ID(index int) any {
	if index < 0 || index >= len(m.rows) {
		return nil
	}
	item := m.items[m.rows[index]].Item
	return rowKey{item.Series, item.Key}
}

func (m *tableModel) Sort(column int, order walk.SortOrder) error {
	if column < 0 || column >= len(displayRow{}.Cells) {
		return m.SorterBase.Sort(column, order)
	}
	slices.SortFunc(m.rows, func(left, right int) int {
		a, b := &m.items[left], &m.items[right]
		var ordering int
		switch {
		case column == 1:
			ordering = cmp.Compare(a.Rank, b.Rank)
		case column == 2 && (a.Before >= 0 || b.Before >= 0):
			ordering = cmp.Compare(a.Before, b.Before)
		case column == 3 && (a.After >= 0 || b.After >= 0):
			ordering = cmp.Compare(a.After, b.After)
		default:
			ordering = strings.Compare(strings.ToLower(a.Cells[column]), strings.ToLower(b.Cells[column]))
		}
		if ordering == 0 {
			if byName := strings.Compare(a.Item.Name, b.Item.Name); byName != 0 {
				return byName
			}
			return cmp.Compare(left, right)
		}
		if order == walk.SortDescending {
			return -ordering
		}
		return ordering
	})
	return m.SorterBase.Sort(column, order)
}

type rankedPane struct {
	ctx            context.Context
	model          tableModel
	table          *walk.TableView
	empty          *walk.Label
	chartHeading   *walk.Label
	detail         *walk.TextEdit
	bars           [5]*walk.ProgressBar
	barLabels      [5]*walk.Label
	action         *walk.PushButton
	copy           *walk.PushButton
	inspect        *walk.PushButton
	projectAction  bool
	inspectSeries  string
	inspectReady   bool
	inspectBlocked bool
	sorted         bool
	emptyText      string
}

func (p *rankedPane) layout(tableName, chartTitle string, columns [4]string, openText, inspectText string,
	projectAction bool, open, inspect func()) d.Widget {
	p.projectAction = projectAction
	chart := []d.Widget{
		d.Label{AssignTo: &p.chartHeading, Text: chartTitle, Font: d.Font{Bold: true}},
	}
	for slot := range p.bars {
		chart = append(chart,
			d.Label{AssignTo: &p.barLabels[slot], Text: " ", EllipsisMode: d.EllipsisEnd, NoPrefix: true},
			d.ProgressBar{AssignTo: &p.bars[slot], MaxValue: 1000, MinSize: d.Size{Height: 7}, MaxSize: d.Size{Height: 7}},
		)
	}
	chart = append(chart,
		d.Label{Text: "Details", Font: d.Font{Bold: true}},
		d.TextEdit{AssignTo: &p.detail, ReadOnly: true, VScroll: true,
			Accessibility: d.Accessibility{Name: "Selected item details"},
			MinSize:       d.Size{Height: 85}, Text: "Select an item to see its details."},
	)
	buttons := []d.Widget{
		d.PushButton{AssignTo: &p.action, Text: openText, OnClicked: open, Enabled: false},
		d.PushButton{AssignTo: &p.copy, Text: "Copy", Enabled: false, OnClicked: func() {
			if row, ok := p.selected(); ok {
				value := row.Item.Path
				if value == "" {
					value = row.Item.Name
				}
				if err := walk.Clipboard().SetText(value); err != nil {
					logDiagnostic(p.ctx, slog.LevelWarn, "gui.copy_failed", slog.String("error_kind", diagnosticErrorKind(err)))
					_ = p.detail.SetText(row.Detail + "\r\n\r\nCould not copy. Try again.")
				} else {
					_ = p.copy.SetText("Copied")
				}
			}
		}},
	}
	if inspect != nil {
		buttons = append(buttons, d.PushButton{AssignTo: &p.inspect, Text: inspectText, OnClicked: inspect, Enabled: false})
	}
	chart = append(chart, d.Composite{Layout: d.HBox{MarginsZero: true}, Children: buttons})
	return d.Composite{Layout: d.HBox{MarginsZero: true, Spacing: 10}, Children: []d.Widget{
		d.Composite{StretchFactor: 1, Layout: d.VBox{MarginsZero: true, Spacing: 6}, Children: []d.Widget{
			d.Label{AssignTo: &p.empty, Visible: false, NoPrefix: true, EllipsisMode: d.EllipsisEnd,
				Font: d.Font{Bold: true}},
			d.TableView{
				AssignTo: &p.table, Model: &p.model, AlternatingRowBG: true, StretchFactor: 1,
				Accessibility:       d.Accessibility{Name: tableName},
				LastColumnStretched: true, SelectionHiddenWithoutFocus: false,
				Columns: []d.TableViewColumn{
					{Title: columns[0], Width: 260},
					{Title: columns[1], Width: 108, Alignment: d.AlignFar},
					{Title: columns[2], Width: 115},
					{Title: columns[3], Width: 122},
				},
				OnCurrentIndexChanged: p.showDetail,
				OnItemActivated: func() {
					if p.action != nil && p.action.Enabled() {
						open()
					}
				},
			},
		}},
		d.Composite{Layout: d.VBox{MarginsZero: true, Spacing: 7}, Children: chart,
			MinSize: d.Size{Width: 316}, MaxSize: d.Size{Width: 328}},
	}}
}

func (p *rankedPane) selected() (displayRow, bool) {
	if p.table != nil {
		if index := p.table.CurrentIndex(); index >= 0 && index < len(p.model.rows) {
			return p.model.items[p.model.rows[index]], true
		}
	}
	return displayRow{}, false
}

func (p *rankedPane) restoreSelection(key rowKey) {
	for index, source := range p.model.rows {
		row := &p.model.items[source]
		if row.Item.Series == key.Series && row.Item.Key == key.Key {
			_ = p.table.SetCurrentIndex(index)
			return
		}
	}
}

func (p *rankedPane) showDetail() {
	if p.detail == nil || p.action == nil || p.copy == nil {
		return
	}
	row, ok := p.selected()
	p.copy.SetEnabled(ok)
	_ = p.copy.SetText("Copy")
	p.action.SetEnabled(ok && ((!p.projectAction && row.Item.Path != "") || (p.projectAction && row.Item.Series == projects)))
	if p.inspect != nil {
		p.inspect.SetEnabled(ok && row.Item.Directory && !p.inspectBlocked &&
			(p.inspectSeries == "" || (p.inspectReady && row.Item.Series == p.inspectSeries)))
	}
	text := "Select an item to see its details."
	if ok {
		text = row.Detail
	}
	_ = p.detail.SetText(text)
}

func (p *rankedPane) show(rows []displayRow, query, empty string) {
	if p.table == nil {
		return
	}
	previous, keep := p.selected()
	p.model.items, p.emptyText = rows, empty
	p.applyFilter(query, previous, keep)
}

func (p *rankedPane) search(box *walk.LineEdit, view string) {
	if p.table == nil || box == nil || len(p.model.items) == 0 {
		return
	}
	started := time.Now()
	previous, keep := p.selected()
	p.applyFilter(box.Text(), previous, keep)
	logRender(p.ctx, view+".search", started, len(p.model.items), len(p.model.rows))
}

func (p *rankedPane) applyFilter(query string, previous displayRow, keep bool) {
	filter := parseFilter(query)
	rows := p.model.rows[:0]
	for index := range p.model.items {
		row := &p.model.items[index]
		if filter.matches(row.Item, row.Detail) {
			rows = append(rows, index)
		}
	}
	p.model.rows = rows
	empty := p.emptyText
	if len(rows) == 0 && len(p.model.items) > 0 {
		empty = "No matches. Try a different search."
	}
	_ = p.empty.SetText(empty)
	p.empty.SetVisible(len(rows) == 0)
	column, order := p.model.SortedColumn(), p.model.SortOrder()
	if !p.sorted || column < 0 {
		column, order = 1, walk.SortDescending
		p.sorted = true
	}
	_ = p.model.Sort(column, order)
	p.model.PublishRowsReset()
	next := -1
	if len(p.model.rows) > 0 {
		next = 0
		for index, source := range p.model.rows {
			row := &p.model.items[source]
			if keep && row.Item.Series == previous.Item.Series && row.Item.Key == previous.Item.Key {
				next = index
				break
			}
		}
	}
	_ = p.table.SetCurrentIndex(next)
	p.showDetail()
	var ranked [len(p.bars)]int
	count := 0
	for _, source := range rows {
		rank := p.model.items[source].Rank
		if rank <= 0 {
			continue
		}
		slot := count
		for slot > 0 && p.model.items[ranked[slot-1]].Rank < rank {
			slot--
		}
		if slot < len(ranked) {
			count = min(count+1, len(ranked))
			copy(ranked[slot+1:count], ranked[slot:count-1])
			ranked[slot] = source
		}
	}
	p.chartHeading.SetVisible(count != 0)
	for slot, bar := range p.bars {
		text, value := " ", 0
		visible := slot < count
		if visible {
			row := p.model.items[ranked[slot]]
			text = row.Cells[1] + "   " + row.Item.Name
			value = max(1, int(float64(row.Rank)*1000/float64(p.model.items[ranked[0]].Rank)))
		}
		_ = p.barLabels[slot].SetText(text)
		bar.SetValue(value)
		p.barLabels[slot].SetVisible(visible)
		bar.SetVisible(visible)
	}
}

type captureTask struct {
	scope    string
	ctx      context.Context
	started  time.Time
	cancel   context.CancelFunc
	stopping bool
}

type fileViewState struct {
	saved snapshot
	mode  string
	query string
	row   rowKey
}

type changeFocus struct {
	path  string
	query string
	row   rowKey
}

type guiRead struct{ cancel context.CancelFunc }

func cancelGUIRead(slot **guiRead) {
	if current := *slot; current != nil {
		*slot = nil
		current.cancel()
	}
}

// Only the UI thread owns the slots. Results from superseded reads are discarded.
func readForGUI[T any](u *gui, slot **guiRead, event string, read func(context.Context) (T, error), apply func(T, error)) {
	cancelGUIRead(slot)
	ctx, cancel := context.WithCancel(diagnosticOperation(u.ctx))
	request := &guiRead{cancel: cancel}
	*slot = request
	started := time.Now()
	go func() {
		defer logPanic(ctx, event)
		value, err := read(ctx)
		if ctx.Err() != nil {
			logDuration(ctx, event, started, ctx.Err())
			return
		}
		queued := time.Now()
		u.dispatch(func() {
			if *slot != request || ctx.Err() != nil {
				logDuration(ctx, event, started, context.Canceled)
				return
			}
			*slot = nil
			defer cancel()
			applying := time.Now()
			apply(value, err)
			logDuration(ctx, event, started, err, slog.Float64("ui_queue_ms", milliseconds(applying.Sub(queued))),
				slog.Float64("apply_ms", milliseconds(time.Since(applying))))
		})
	}()
}

type gui struct {
	idx              *index
	indexPath        string
	indexOpening     bool
	opened           <-chan *index
	ctx              context.Context
	cancel           context.CancelFunc
	dispatchMu       sync.Mutex
	window           *walk.MainWindow
	tabs             *walk.TabWidget
	driveLabels      []*walk.Label
	driveBars        []*walk.ProgressBar
	filePane         rankedPane
	dockerPane       rankedPane
	changePane       rankedPane
	files            snapshot
	docker           snapshot
	back             []fileViewState
	drives           []string
	fileScope        *walk.ComboBox
	fileMode         *walk.ComboBox
	fileFilter       *walk.LineEdit
	fileStatus       *walk.Label
	scanButton       *walk.PushButton
	browse           *walk.PushButton
	backButton       *walk.PushButton
	settingFileScope bool
	dockerMode       *walk.ComboBox
	dockerFilter     *walk.LineEdit
	idleOnly         *walk.CheckBox
	dockerStatus     *walk.Label
	dockerButton     *walk.PushButton
	dockerCards      [5]*walk.Label
	dockerCaptions   [5]*walk.Label
	dockerOpened     bool
	dockerProject    string
	changeScope      *walk.ComboBox
	changeMode       *walk.ComboBox
	period           *walk.ComboBox
	minimum          *walk.ComboBox
	direction        *walk.ComboBox
	changeFilter     *walk.LineEdit
	changeStatus     *walk.Label
	driveDelta       *walk.Label
	scopeDelta       *walk.Label
	scopes           []snapshot
	changingChoices  bool
	comparison       comparison
	compareLoad      *guiRead
	fileLoad         *guiRead
	scopeLoad        *guiRead
	dueLoad          *guiRead
	focused          *changeFocus
	changePath       *walk.Label
	changeBreadcrumb *walk.Composite
	changeCapture    *walk.PushButton
	activity         *walk.Label
	activityBar      *walk.ProgressBar
	cancelButton     *walk.PushButton
	captureMenu      *walk.Action
	cancelMenu       *walk.Action
	findMenu         *walk.Action
	reloadMenu       *walk.Action
	active           *captureTask
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

func runGUI(parent context.Context, indexPath, diagnosticsDir string, custom bool) (err error) {
	started := time.Now()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ui := &gui{indexPath: indexPath, ctx: ctx, cancel: cancel, drives: driveRoots(), files: snapshot{Label: quickScope},
		filePane: rankedPane{ctx: ctx}, dockerPane: rankedPane{ctx: ctx}, changePane: rankedPane{ctx: ctx}}
	ui.driveLabels = make([]*walk.Label, len(ui.drives))
	ui.driveBars = make([]*walk.ProgressBar, len(ui.drives))
	var cards []d.Widget
	for index, root := range ui.drives {
		cards = append(cards, d.GroupBox{
			Title: root[:2], StretchFactor: 1,
			Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 6, Right: 10, Bottom: 6}, Spacing: 5},
			Children: []d.Widget{
				d.Label{AssignTo: &ui.driveLabels[index], Text: "Reading free space…", NoPrefix: true},
				d.ProgressBar{AssignTo: &ui.driveBars[index], MaxValue: 1000, MinSize: d.Size{Height: 8}, MaxSize: d.Size{Height: 8}},
			},
		})
	}
	mcpPage, err := ui.mcpPage(indexPath, custom)
	if err != nil {
		return err
	}
	err = (d.MainWindow{
		AssignTo: &ui.window, Title: "Space Review",
		Size: d.Size{Width: 1160, Height: 780}, MinSize: d.Size{Width: 935, Height: 670},
		Font:   d.Font{Family: "Segoe UI", PointSize: 11},
		Layout: d.VBox{Margins: d.Margins{Left: 16, Top: 14, Right: 16, Bottom: 12}, Spacing: 9},
		MenuItems: []d.MenuItem{
			d.Menu{Text: "&File", Items: []d.MenuItem{
				d.Action{AssignTo: &ui.captureMenu, Text: "Capture &now", Shortcut: d.Shortcut{Key: walk.KeyF5}, OnTriggered: ui.captureCurrent},
				d.Action{AssignTo: &ui.cancelMenu, Text: "&Cancel capture", Enabled: false, OnTriggered: ui.cancelActive},
			}},
			d.Menu{Text: "&View", Items: []d.MenuItem{
				d.Action{AssignTo: &ui.findMenu, Text: "&Find", Shortcut: d.Shortcut{Modifiers: walk.ModControl, Key: walk.KeyF}, OnTriggered: ui.focusSearch},
				d.Action{AssignTo: &ui.reloadMenu, Text: "Reload &history", OnTriggered: ui.updateScopes},
			}},
			d.Menu{Text: "&Help", Items: []d.MenuItem{
				d.Action{Text: "Open &diagnostics folder", Enabled: diagnosticsDir != "", OnTriggered: func() {
					ui.openDiagnosticsFolder(diagnosticsDir)
				}},
			}},
		},
		Children: []d.Widget{
			d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
				d.Label{Text: "Space Review", Font: d.Font{PointSize: 22, Bold: true}},
				d.HSpacer{}, d.Label{Text: "Local history  ·  Read-only scans"},
			}},
			d.Composite{Layout: d.HBox{MarginsZero: true, Spacing: 8}, Children: cards},
			d.TabWidget{AssignTo: &ui.tabs, StretchFactor: 1, OnCurrentIndexChanged: ui.tabChanged,
				Pages: []d.TabPage{ui.changesPage(), ui.filesPage(), ui.dockerPage(), mcpPage}},
			d.Composite{Layout: d.HBox{MarginsZero: true, Spacing: 8}, Children: []d.Widget{
				d.Label{AssignTo: &ui.activity, Text: "Opening local index…", NoPrefix: true, EllipsisMode: d.EllipsisEnd, StretchFactor: 1},
				d.ProgressBar{AssignTo: &ui.activityBar, Visible: false, MinSize: d.Size{Width: 110, Height: 8},
					MaxSize: d.Size{Width: 110, Height: 8}, Accessibility: d.Accessibility{Name: "Capture progress"}},
				d.PushButton{AssignTo: &ui.cancelButton, Text: "Cancel capture", Visible: false, OnClicked: ui.cancelActive},
			}},
		},
	}).Create()
	logDuration(ctx, "gui.window_create", started, err)
	if err != nil {
		return err
	}
	for _, pane := range []*rankedPane{&ui.changePane, &ui.filePane, &ui.dockerPane} {
		pane.table.SetRestoringCurrentItemOnReset(true)
		pane.table.ColumnClicked().Attach(func(_ int) { pane.showDetail() })
	}
	ui.window.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		logDiagnostic(ctx, slog.LevelInfo, "gui.close", slog.Bool("capture_active", ui.active != nil))
		ui.stopDispatch()
	})
	ui.renderFiles()
	ui.renderDocker()
	ui.renderChanges()
	ui.openLocalIndex()
	go func() {
		defer logPanic(ctx, "gui.scheduler")
		ui.scheduleCaptures()
	}()
	ui.window.Run()
	ui.stopDispatch()
	if idx := <-ui.opened; idx != nil {
		_ = idx.db.Close()
	}
	return nil
}

func (u *gui) openLocalIndex() {
	if u.idx != nil || u.indexOpening {
		return
	}
	u.indexOpening = true
	u.setActivity("Opening local index…")
	u.updateActions()
	opened := make(chan *index, 1)
	u.opened = opened
	go u.loadStartup(u.indexPath, opened)
}

func (u *gui) loadStartup(indexPath string, opened chan<- *index) {
	defer logPanic(u.ctx, "gui.startup")
	started := time.Now()
	idx, err := openIndex(u.ctx, indexPath)
	opened <- idx
	if u.ctx.Err() != nil {
		return
	}
	disks := diskState(u.drives)
	var list []snapshot
	files := snapshot{Label: quickScope}
	var docker snapshot
	if err == nil {
		list, err = idx.list(u.ctx)
	}
	if err == nil {
		if saved, readErr := idx.latest(u.ctx, savedScopeKey(quickScope)); readErr == nil {
			files = saved
		}
		for _, saved := range list {
			if saved.Label == dockerScope {
				docker, _ = idx.latest(u.ctx, saved.Scope)
				break
			}
		}
	}
	if u.ctx.Err() != nil {
		return
	}
	u.dispatch(func() {
		u.indexOpening = false
		u.idx, u.files, u.docker = idx, files, docker
		u.showDrives(disks)
		u.renderFiles()
		u.renderDocker()
		u.applyScopes(list)
		u.updateActions()
		logDuration(u.ctx, "gui.startup", started, err, slog.Bool("saved_files", files.ID != 0),
			slog.Bool("saved_docker", docker.ID != 0), slog.Int("fixed_drives", len(disks)))
		if err != nil {
			u.setActivity("Could not load the local index. Use View → Reload history to retry. " + err.Error())
			return
		}
		u.setActivity("Ready")
		if refreshAtStartup(files, disks) {
			logDiagnostic(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "startup"), slog.String("scope_type", "quick"))
			u.startFiles(quickScope, "", false)
		} else if u.tabs.CurrentIndex() == tabDocker {
			u.tabChanged()
		}
	})
}

func (u *gui) filesPage() d.TabPage {
	return d.TabPage{Title: "Files", Layout: d.VBox{}, Children: []d.Widget{
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Location"},
			d.ComboBox{AssignTo: &u.fileScope, Model: append([]string{quickScope}, u.drives...), CurrentIndex: 0,
				MinSize: d.Size{Width: 205}, Accessibility: d.Accessibility{Name: "File location"},
				OnCurrentIndexChanged: u.selectFileScope},
			d.PushButton{AssignTo: &u.browse, Text: "Choose folder…", OnClicked: u.chooseFolder},
			d.PushButton{AssignTo: &u.backButton, Text: "Back", Enabled: false, OnClicked: u.goBack},
			d.HSpacer{},
			d.PushButton{AssignTo: &u.scanButton, Text: "Scan now", ToolTipText: "Scan this location (F5)", OnClicked: u.scanClick},
		}},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Show"},
			d.ComboBox{AssignTo: &u.fileMode, Model: supportedSeries("files", false), CurrentIndex: 0,
				MinSize: d.Size{Width: 165}, OnCurrentIndexChanged: u.renderFiles},
			d.Label{Text: "Search"},
			d.LineEdit{AssignTo: &u.fileFilter, MinSize: d.Size{Width: 175}, OnTextChanged: func() { u.filePane.search(u.fileFilter, "files") },
				Accessibility: d.Accessibility{Name: "Search files"}},
			d.HSpacer{},
		}},
		u.filePane.layout("File usage", "Largest items", [4]string{"Location", "On disk", "Category", "Last modified"},
			"Open in Explorer", "Inspect", false, func() { u.openSelection(&u.filePane) }, u.inspectFolder),
		d.Label{AssignTo: &u.fileStatus, Text: "Ready", NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Label{Text: "Quick review captures daily while selected and the app is open. File allocation is counted once per scan."},
	}}
}

func (u *gui) dockerPage() d.TabPage {
	var cards []d.Widget
	for index, name := range []string{"Windows disk", images, containers, volumes, buildCache} {
		cards = append(cards, d.GroupBox{Title: name, StretchFactor: 1,
			MinSize: d.Size{Width: 155},
			Layout:  d.VBox{Margins: d.Margins{Left: 10, Top: 3, Right: 10, Bottom: 5}},
			Children: []d.Widget{
				d.Label{AssignTo: &u.dockerCards[index], Text: "—", Font: d.Font{PointSize: 15, Bold: true}},
				d.Label{AssignTo: &u.dockerCaptions[index], Text: " "},
			}})
	}
	return d.TabPage{Title: "Docker", Layout: d.VBox{}, Children: []d.Widget{
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: cards},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Show"},
			d.ComboBox{AssignTo: &u.dockerMode, Model: supportedSeries("docker", false), CurrentIndex: 0,
				MinSize: d.Size{Width: 150}, OnCurrentIndexChanged: u.renderDocker},
			d.Label{Text: "Search"},
			d.LineEdit{AssignTo: &u.dockerFilter, MinSize: d.Size{Width: 175}, OnTextChanged: func() { u.dockerPane.search(u.dockerFilter, "docker") },
				Accessibility: d.Accessibility{Name: "Search Docker resources"}},
			d.CheckBox{AssignTo: &u.idleOnly, Text: "Idle only", OnCheckedChanged: u.renderDocker},
			d.HSpacer{}, d.PushButton{AssignTo: &u.dockerButton, Text: "Refresh Docker",
				ToolTipText: "Capture current Docker usage (F5)", OnClicked: u.startDocker},
		}},
		u.dockerPane.layout("Docker resources", "Largest items", [4]string{"Resource", "Size", "Type", "Activity"},
			"Show project items", "", true, u.inspectProject, nil),
		d.Label{AssignTo: &u.dockerStatus, Text: "Refresh Docker to measure the local engine.", NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Label{Text: "Projects show writable layers and volumes. The Windows disk has its own total."},
	}}
}

func (u *gui) changesPage() d.TabPage {
	u.changePane.inspectSeries = folders
	return d.TabPage{Title: "Changes", Layout: d.VBox{}, Children: []d.Widget{
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Location"},
			d.ComboBox{AssignTo: &u.changeScope, Model: []string{quickScope}, CurrentIndex: 0,
				MinSize: d.Size{Width: 205}, OnCurrentIndexChanged: u.chooseChangeScope,
				Accessibility: d.Accessibility{Name: "Location to compare"}},
			d.Label{Text: "Compare"},
			d.ComboBox{AssignTo: &u.period, Model: periods, CurrentIndex: 0,
				MinSize: d.Size{Width: 133}, OnCurrentIndexChanged: u.loadComparison,
				Accessibility: d.Accessibility{Name: "Comparison period"}},
			d.HSpacer{}, d.PushButton{AssignTo: &u.changeCapture, Text: "Scan now",
				ToolTipText: "Save a fresh measurement (F5)", OnClicked: u.captureChanges},
		}},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Show"},
			d.ComboBox{AssignTo: &u.changeMode, Model: supportedSeries("files", true), CurrentIndex: 0,
				MinSize: d.Size{Width: 155}, OnCurrentIndexChanged: u.changeModeChanged,
				Accessibility: d.Accessibility{Name: "Result type"}},
			d.Label{Text: "Minimum"},
			d.ComboBox{AssignTo: &u.minimum, Model: cutoffs, CurrentIndex: 1,
				MinSize: d.Size{Width: 82}, OnCurrentIndexChanged: u.renderChanges,
				Accessibility: d.Accessibility{Name: "Minimum change"}},
			d.ComboBox{AssignTo: &u.direction, Model: []string{"Both directions", "Growth", "Freed"}, CurrentIndex: 0,
				MinSize: d.Size{Width: 118}, OnCurrentIndexChanged: u.renderChanges,
				Accessibility: d.Accessibility{Name: "Change direction"}},
			d.Label{Text: "Search"}, d.LineEdit{AssignTo: &u.changeFilter, MinSize: d.Size{Width: 120},
				OnTextChanged: u.searchChanges, Accessibility: d.Accessibility{Name: "Search changes"}},
			d.HSpacer{},
		}},
		d.Label{AssignTo: &u.driveDelta, Text: "What changed?",
			Font: d.Font{PointSize: 12, Bold: true}, NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Label{AssignTo: &u.scopeDelta, Text: " ", NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Composite{AssignTo: &u.changeBreadcrumb, Visible: false, Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.PushButton{Text: "Back to folders", OnClicked: u.backChanges},
			d.Label{AssignTo: &u.changePath, NoPrefix: true, EllipsisMode: d.EllipsisPath, StretchFactor: 1},
		}},
		u.changePane.layout("Storage changes", "Biggest changes", [4]string{"Location or resource", "Change", "Before", "After"},
			"Open in Explorer", "Inspect", false, func() { u.openSelection(&u.changePane) }, u.inspectChange),
		d.Label{AssignTo: &u.changeStatus, Text: "Run a scan to create a baseline.", NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Label{Text: "≥ means at least that much changed. Large files and subfolders are tracked from 64 MiB."},
	}}
}

func (u *gui) mcpPage(indexPath string, custom bool) (d.TabPage, error) {
	executable, err := os.Executable()
	if err != nil {
		return d.TabPage{}, err
	}
	args := []string{"--mcp"}
	if custom {
		args = append(args, "--index", indexPath)
	}
	config, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"space-review": map[string]any{"command": executable, "args": args},
	}}, "", "  ")
	if err != nil {
		return d.TabPage{}, err
	}
	text := strings.ReplaceAll(string(config), "\n", "\r\n")
	var copyStatus *walk.Label
	return d.TabPage{Title: "MCP", Layout: d.VBox{}, Children: []d.Widget{
		d.Label{Text: "Connect a local MCP client", Font: d.Font{PointSize: 16, Bold: true}},
		d.TextLabel{Text: "Copy the configuration into your MCP client. It launches this executable and reads the same saved history.",
			MinSize: d.Size{Width: 440}},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.PushButton{Text: "Copy configuration", OnClicked: func() {
				message := "Configuration copied."
				if err := walk.Clipboard().SetText(text); err != nil {
					message = "Could not copy. Select the configuration to copy it manually."
				}
				_ = copyStatus.SetText(message)
			}},
			d.Label{AssignTo: &copyStatus, NoPrefix: true},
			d.HSpacer{},
		}},
		d.TextEdit{ReadOnly: true, Text: text, Font: d.Font{Family: "Consolas", PointSize: 10},
			Accessibility: d.Accessibility{Name: "MCP configuration"},
			MinSize:       d.Size{Height: 165}, VScroll: true, HScroll: true},
		d.Label{Text: "Tools", Font: d.Font{Bold: true}},
		d.TextLabel{Text: "list_storage_scopes, largest_storage_items, storage_changes, capture_storage", MinSize: d.Size{Width: 440}},
		d.Label{Text: "SQLite index", Font: d.Font{Bold: true}},
		d.TextLabel{Text: indexPath, NoPrefix: true, MinSize: d.Size{Width: 440}},
		d.VSpacer{},
	}}, nil
}

func (u *gui) showDrives(disks []driveState) {
	for index, label := range u.driveLabels {
		position := slices.IndexFunc(disks, func(d driveState) bool { return d.Root == u.drives[index] })
		if position < 0 || disks[position].Total <= 0 {
			_ = label.SetText("Free space unavailable")
			u.driveBars[index].SetValue(0)
			continue
		}
		drive := disks[position]
		_ = label.SetText(sizeText(drive.Free) + " free of " + sizeText(drive.Total))
		u.driveBars[index].SetValue(int(float64(drive.Total-drive.Free) * 1000 / float64(drive.Total)))
	}
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

func (u *gui) setFileScope(scope string) {
	u.settingFileScope = true
	defer func() { u.settingFileScope = false }()
	options := append([]string{quickScope}, u.drives...)
	for _, saved := range u.scopes {
		if saved.Coverage.ScopeKind == "files" && volumeRoot(saved.Label) != "" && !slices.Contains(options, saved.Label) {
			options = append(options, saved.Label)
		}
	}
	if !slices.Contains(options, scope) {
		options = append(options, scope)
	}
	if model, _ := u.fileScope.Model().([]string); !slices.Equal(model, options) {
		_ = u.fileScope.SetModel(options)
	}
	if selected := slices.Index(options, scope); selected != u.fileScope.CurrentIndex() {
		_ = u.fileScope.SetCurrentIndex(selected)
	}
}

func (u *gui) selectFileScope() {
	if u.settingFileScope || u.fileStatus == nil {
		return
	}
	scope := combo(u.fileScope, quickScope)
	if strings.EqualFold(scope, u.files.Label) {
		return
	}
	u.loadFileScope(scope)
}

func (u *gui) loadFileScope(scope string) {
	idx := u.idx
	if idx == nil {
		return
	}
	u.files = snapshot{Label: scope}
	readForGUI(u, &u.fileLoad, "gui.file_location", func(ctx context.Context) (snapshot, error) {
		return idx.latest(ctx, savedScopeKey(scope))
	}, func(saved snapshot, err error) {
		if err == nil {
			u.files = saved
		}
		u.renderFiles()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			_ = u.fileStatus.SetText("Could not load the saved scan. " + err.Error())
		}
	})
	u.renderFiles()
}

func (u *gui) scanClick() {
	scope := combo(u.fileScope, quickScope)
	kind := ""
	if strings.EqualFold(scope, u.files.Label) {
		kind = u.files.Coverage.Kind
	}
	u.startFiles(scope, kind, false)
}

func (u *gui) startFiles(scope, kind string, remember bool) {
	idx := u.idx
	if idx == nil || u.active != nil {
		return
	}
	if scope != quickScope {
		scope = filepath.Clean(scope)
	}
	if remember && u.files.Label != "" && !strings.EqualFold(scope, u.files.Label) {
		state := fileViewState{saved: u.files, mode: combo(u.fileMode, folders), query: u.fileFilter.Text()}
		if row, ok := u.filePane.selected(); ok {
			state.row = rowKey{row.Item.Series, row.Item.Key}
		}
		u.back = append(u.back, state)
		if len(u.back) > 8 {
			u.back = u.back[len(u.back)-8:]
		}
	}
	u.setFileScope(scope)
	if !strings.EqualFold(scope, u.files.Label) {
		u.loadFileScope(scope)
	}
	if remember {
		_ = u.fileMode.SetCurrentIndex(0)
		_ = u.fileFilter.SetText("")
	}
	ctx := u.beginCapture(scope, "Scanning "+scope+"…")
	task := u.active
	u.renderFiles()
	go func() {
		defer logPanic(ctx, "gui.files_worker")
		saved, err := runCapture(ctx, idx, scope, kind, func(_ string, checked int64) {
			u.dispatch(func() {
				if u.active == task && ctx.Err() == nil {
					u.setActivity(fmt.Sprintf("Scanning %s  ·  %d files checked", scope, checked))
				}
			})
		})
		if u.ctx.Err() != nil {
			return
		}
		var disks []driveState
		if err == nil {
			disks = diskState(u.drives)
		}
		u.applyCapture(task, err, func() {
			if err != nil {
				u.renderFiles()
				return
			}
			cancelGUIRead(&u.fileLoad)
			u.files = saved
			u.setFileScope(saved.Label)
			u.showDrives(disks)
			u.renderFiles()
			u.updateScopes()
		})
	}()
}

func (u *gui) chooseFolder() {
	if u.idx == nil || u.active != nil {
		return
	}
	dialog := walk.FileDialog{Title: "Choose a folder on a local fixed drive"}
	if accepted, err := dialog.ShowBrowseFolder(u.window); err != nil {
		logDiagnostic(u.ctx, slog.LevelWarn, "gui.folder_dialog_failed", slog.String("error_kind", diagnosticErrorKind(err)))
		u.setActivity("Could not open the folder chooser. " + err.Error())
	} else if accepted {
		u.startFiles(dialog.FilePath, "", true)
	}
}

func (u *gui) inspectFolder() {
	if row, ok := u.filePane.selected(); ok && row.Item.Directory {
		u.startFiles(row.Item.Path, row.Item.Kind, true)
	}
}

func (u *gui) goBack() {
	if len(u.back) == 0 || u.fileCaptureActive() {
		return
	}
	cancelGUIRead(&u.fileLoad)
	state := u.back[len(u.back)-1]
	u.files = state.saved
	u.back = u.back[:len(u.back)-1]
	u.setFileScope(u.files.Label)
	_ = u.fileMode.SetCurrentIndex(max(0, slices.Index(supportedSeries("files", false), state.mode)))
	_ = u.fileFilter.SetText(state.query)
	u.renderFiles()
	u.filePane.restoreSelection(state.row)
	u.updateActions()
}

func (u *gui) renderFiles() {
	if u.fileFilter == nil || u.fileStatus == nil {
		return
	}
	started := time.Now()
	defer func() { logRender(u.ctx, "files", started, len(u.files.Records), len(u.filePane.model.rows)) }()
	series := combo(u.fileMode, folders)
	var rows []displayRow
	for _, item := range u.files.Records {
		if item.Series != series {
			continue
		}
		detail := item.Path + "\r\n\r\n" + sizeText(item.Bytes) + " on disk"
		if item.Directory {
			detail += fmt.Sprintf("  ·  %d files", item.Files)
		}
		detail += "\r\n" + kindNote(item.Kind)
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: -1, After: -1, Detail: detail,
			Cells: [4]string{item.Name, sizeText(item.Bytes), item.Kind, fileDate(item.Modified)}})
	}
	empty := "No items in this view."
	if u.files.ID == 0 {
		empty = "Scan this location to see what uses space."
		if u.fileCaptureActive() {
			empty = "Measuring this location…"
		} else if u.fileLoad != nil {
			empty = "Loading saved scan…"
		}
		_ = u.fileStatus.SetText(" ")
	}
	u.filePane.show(rows, u.fileFilter.Text(), empty)
	if u.files.ID != 0 {
		if series == subfolders && !u.files.Coverage.Subfolders {
			_ = u.fileStatus.SetText("Scan this scope to index its subfolders.")
			return
		}
		var total int64
		for _, item := range u.files.Records {
			if item.Series == folders {
				total += item.Bytes
			}
		}
		c := u.files.Coverage
		_ = u.fileStatus.SetText(fmt.Sprintf("Saved %s  ·  %s  ·  %s measured  ·  %d files  ·  %d unreadable  ·  %d repeated links",
			timeText(u.files.Captured), u.files.Label, sizeText(total), c.Checked, len(c.Gaps), c.Shared))
	}
}

func dailyCaptureDue(s snapshot, now time.Time) bool {
	return s.ID != 0 && now.Sub(time.UnixMilli(s.Captured)) >= 24*time.Hour
}

func (u *gui) scheduleCaptures() {
	ticker := time.NewTicker(15 * time.Minute)
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

func (u *gui) captureDue(now time.Time) {
	idx := u.idx
	if idx == nil || u.ctx.Err() != nil || u.active != nil || u.dueLoad != nil {
		return
	}
	dockerDue := u.tabs.CurrentIndex() == tabDocker && dailyCaptureDue(u.docker, now)
	filesDue := combo(u.fileScope, "") == quickScope && u.files.Label == quickScope && dailyCaptureDue(u.files, now)
	if !dockerDue && !filesDue {
		return
	}
	dockerScope, filesScope := u.docker.Scope, u.files.Scope
	type scheduled struct{ docker, files snapshot }
	readForGUI(u, &u.dueLoad, "gui.scheduled_lookup", func(ctx context.Context) (scheduled, error) {
		var saved scheduled
		var dockerErr, filesErr error
		if dockerDue {
			saved.docker, dockerErr = idx.latest(ctx, dockerScope)
		}
		if filesDue {
			saved.files, filesErr = idx.latest(ctx, filesScope)
		}
		return saved, errors.Join(dockerErr, filesErr)
	}, func(saved scheduled, _ error) {
		if u.active != nil {
			return
		}
		if dockerDue && u.tabs.CurrentIndex() == tabDocker && u.docker.Scope == dockerScope {
			if u.refreshDue(&u.docker, saved.docker, now, u.renderDocker) {
				logDiagnostic(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "daily"), slog.String("scope_type", "docker"))
				u.startDocker()
				return
			}
		}
		if filesDue && combo(u.fileScope, "") == quickScope && u.files.Scope == filesScope {
			if u.refreshDue(&u.files, saved.files, now, u.renderFiles) {
				logDiagnostic(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "daily"), slog.String("scope_type", "quick"))
				u.startFiles(quickScope, "", false)
			}
		}
	})
}

func (u *gui) refreshDue(saved *snapshot, latest snapshot, now time.Time, render func()) bool {
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

func closestLocalFolder(path string) (string, error) {
	for {
		valid, err := localFolder(path)
		parent := filepath.Dir(path)
		if !os.IsNotExist(err) || parent == path || path == "" {
			return valid, err
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
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return err
	}
	defer windows.CoUninitialize()
	verb, _ := windows.UTF16PtrFromString("explore")
	return windows.ShellExecute(0, verb, folder, nil, nil, 1)
}

func (u *gui) openDiagnosticsFolder(path string) {
	u.openFolder(path, "gui.open_diagnostics", "Diagnostics folder unavailable", false)
}

func (u *gui) openFolder(path, event, title string, findParent bool) {
	ctx := diagnosticOperation(u.ctx)
	go func() {
		defer logPanic(ctx, event)
		started := time.Now()
		var err error
		if findParent {
			path, err = closestLocalFolder(path)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = exploreFolder(path)
		}
		logDuration(ctx, event, started, err)
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
		if u.idx != nil && !u.dockerOpened && u.active == nil {
			u.dockerOpened = true
			if u.docker.ID == 0 || time.Since(time.UnixMilli(u.docker.Captured)) >= 15*time.Minute {
				logDiagnostic(u.ctx, slog.LevelInfo, "gui.auto_capture", slog.String("trigger", "docker_tab"), slog.String("scope_type", "docker"))
				u.startDocker()
			}
		}
	case tabChanges:
		u.updateScopes()
	}
	u.updateActions()
}

func (u *gui) fileCaptureActive() bool {
	return u.active != nil && u.active.scope != dockerScope
}

func (u *gui) updateActions() {
	if u.tabs == nil || u.cancelButton == nil || u.changeCapture == nil || u.captureMenu == nil {
		return
	}
	idle := u.active == nil
	ready := u.idx != nil
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
	_ = u.reloadMenu.SetEnabled(!u.indexOpening)
	search := u.activeSearch()
	_ = u.findMenu.SetEnabled(search != nil && search.Enabled())
	selection := u.changeSelection()
	button := "Scan now"
	if selection.Coverage.ScopeKind == "docker" {
		button = "Refresh Docker"
	} else if selection.ID != 0 && selection.Scope == u.comparison.Latest.Scope && u.comparison.Baseline.ID == 0 {
		button = "Scan again"
	}
	_ = u.changeCapture.SetText(button)
}

func (u *gui) setActivity(message string) {
	_ = u.activity.SetText(message)
	_ = u.activity.SetToolTipText(message)
}

func (u *gui) beginCapture(scope, message string) context.Context {
	ctx, cancel := context.WithCancel(diagnosticOperation(u.ctx))
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
	logDiagnostic(u.active.ctx, slog.LevelInfo, "gui.cancel_requested")
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
		logDuration(task.ctx, "gui.capture", task.started, err, slog.String("scope_type", captureScopeType(task.scope)),
			slog.Float64("ui_queue_ms", milliseconds(applying.Sub(queued))),
			slog.Float64("apply_ms", milliseconds(time.Since(applying))))
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

func (u *gui) captureChanges() {
	selection := u.changeSelection()
	if selection.Coverage.ScopeKind == "docker" {
		u.startDocker()
	} else {
		u.startFiles(selection.Label, selection.Coverage.Kind, false)
	}
}

func (u *gui) startDocker() {
	idx := u.idx
	if idx == nil || u.active != nil {
		return
	}
	u.dockerOpened = true
	ctx := u.beginCapture(dockerScope, "Measuring Docker layers and volumes. This can take a minute.")
	task := u.active
	u.renderDocker()
	go func() {
		defer logPanic(ctx, "gui.docker_worker")
		saved, err := runCapture(ctx, idx, dockerScope, "", nil)
		if u.ctx.Err() != nil {
			return
		}
		u.applyCapture(task, err, func() {
			if err != nil {
				u.renderDocker()
				if !errors.Is(err, context.Canceled) {
					_ = u.dockerStatus.SetText(err.Error())
				}
				return
			}
			u.docker = saved
			u.renderDocker()
			u.updateScopes()
		})
	}()
}

func (u *gui) renderDocker() {
	if u.idleOnly == nil || u.dockerFilter == nil || u.dockerStatus == nil {
		return
	}
	started := time.Now()
	defer func() { logRender(u.ctx, "docker", started, len(u.docker.Records), len(u.dockerPane.model.rows)) }()
	for _, item := range u.docker.Records {
		if item.Series != dockerSum {
			continue
		}
		slot := slices.Index([]string{"host", images, containers, volumes, buildCache}, item.Key)
		if slot >= 0 {
			_ = u.dockerCards[slot].SetText(sizeText(item.Bytes))
			caption := item.Activity
			if slot == 0 {
				caption = "Allocated virtual disk"
			} else if caption == "" {
				caption = "Engine usage"
			}
			_ = u.dockerCaptions[slot].SetText(caption)
		}
	}
	mode := combo(u.dockerMode, projects)
	idle := mode != projects && mode != dockerSum
	u.idleOnly.SetEnabled(idle)
	var rows []displayRow
	for _, item := range u.docker.Records {
		if mode == "Project items" {
			if (item.Series != volumes && item.Series != containers) || item.Group != u.dockerProject {
				continue
			}
		} else if item.Series != mode {
			continue
		}
		if idle && u.idleOnly.Checked() && !item.Idle {
			continue
		}
		detail := item.Name + "\r\n" + sizeText(item.Bytes) + "\r\n\r\n" + item.Detail
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: -1, After: -1, Detail: detail,
			Cells: [4]string{item.Name, sizeText(item.Bytes), item.Kind, item.Activity}})
	}
	empty := "No resources in this view."
	if u.docker.ID == 0 {
		empty = "Refresh Docker to see what uses space."
		if u.active != nil && u.active.scope == dockerScope {
			empty = "Waiting for Docker usage…"
		}
	}
	u.dockerPane.show(rows, u.dockerFilter.Text(), empty)
	if u.docker.ID != 0 {
		_ = u.dockerStatus.SetText(u.docker.Coverage.Engine + "  ·  Saved " + timeText(u.docker.Captured))
	}
}

func (u *gui) inspectProject() {
	if row, ok := u.dockerPane.selected(); ok && row.Item.Series == projects {
		u.dockerProject = row.Item.Group
		options := append(supportedSeries("docker", false), "Project items")
		_ = u.dockerMode.SetModel(options)
		_ = u.dockerMode.SetCurrentIndex(len(options) - 1)
		_ = u.dockerFilter.SetText("")
		u.idleOnly.SetChecked(false)
		u.renderDocker()
	}
}

func (u *gui) updateScopes() {
	idx := u.idx
	if u.changeScope == nil {
		return
	}
	if idx == nil {
		u.openLocalIndex()
		return
	}
	readForGUI(u, &u.scopeLoad, "gui.history", idx.list, func(list []snapshot, err error) {
		if err != nil {
			_ = u.changeStatus.SetText("Could not reload history. " + err.Error())
			return
		}
		u.applyScopes(list)
	})
}

func (u *gui) applyScopes(list []snapshot) {
	previous := u.changeSelection()
	if !slices.ContainsFunc(list, func(s snapshot) bool { return s.Label == quickScope }) {
		list = append(list, snapshot{Label: quickScope, Coverage: coverage{ScopeKind: "files"}})
	}
	u.scopes = list
	u.changingChoices = true
	names := make([]string, 0, len(list))
	index := 0
	for slot, s := range list {
		label := s.Label
		if slices.Contains(names, label) {
			label += " · " + timeText(s.Captured)
		}
		names = append(names, label)
		if (previous.Scope != "" && s.Scope == previous.Scope) ||
			(previous.Scope == "" && s.Label == previous.Label) {
			index = slot
		}
	}
	if model, _ := u.changeScope.Model().([]string); !slices.Equal(model, names) {
		_ = u.changeScope.SetModel(names)
	}
	if index != u.changeScope.CurrentIndex() {
		_ = u.changeScope.SetCurrentIndex(index)
	}
	u.changingChoices = false
	u.setFileScope(combo(u.fileScope, quickScope))
	u.chooseChangeScope()
}

func (u *gui) changeSelection() snapshot {
	if u.changeScope != nil {
		if index := u.changeScope.CurrentIndex(); index >= 0 && index < len(u.scopes) {
			return u.scopes[index]
		}
	}
	return snapshot{Label: quickScope, Coverage: coverage{ScopeKind: "files"}}
}

func (u *gui) chooseChangeScope() {
	if u.changingChoices || u.changeMode == nil || u.changeScope == nil {
		return
	}
	selection := u.changeSelection()
	if u.focused != nil && u.comparison.Latest.Scope != selection.Scope {
		u.clearChangeFocus()
	}
	options := supportedSeries(selection.Coverage.ScopeKind, true)
	if model, _ := u.changeMode.Model().([]string); !slices.Equal(model, options) {
		selected := slices.Index(options, combo(u.changeMode, ""))
		u.changingChoices = true
		_ = u.changeMode.SetModel(options)
		_ = u.changeMode.SetCurrentIndex(max(0, selected))
		u.changingChoices = false
	}
	u.loadComparison()
	u.updateActions()
}

func (u *gui) loadComparison() {
	if u.changingChoices || u.period == nil || u.changeScope == nil || u.changeStatus == nil {
		return
	}
	window := u.period.CurrentIndex()
	if window < 0 {
		return
	}
	scope, days := u.changeSelection().Scope, periodDays[window]
	idx := u.idx
	if scope == "" || idx == nil {
		cancelGUIRead(&u.compareLoad)
		u.comparison = comparison{}
		u.renderChanges()
		return
	}
	if u.comparison.Latest.Scope != scope {
		u.comparison = comparison{}
		u.changePane.show(nil, "", "Loading saved history…")
		_ = u.driveDelta.SetText("What changed?")
		_ = u.scopeDelta.SetText(" ")
	}
	_ = u.changeStatus.SetText("Comparing saved captures…")
	readForGUI(u, &u.compareLoad, "gui.compare", func(ctx context.Context) (comparison, error) {
		return idx.compare(ctx, scope, days)
	}, func(data comparison, err error) {
		if err != nil {
			u.renderChanges()
			if !errors.Is(err, sql.ErrNoRows) {
				_ = u.changeStatus.SetText("Could not load this comparison. " + err.Error())
			}
			return
		}
		u.comparison = data
		u.renderChanges()
	})
}

func (u *gui) enableComparisonControls(ready bool) {
	u.period.SetEnabled(ready)
	u.changeMode.SetEnabled(ready)
	u.minimum.SetEnabled(ready)
	u.direction.SetEnabled(ready)
	u.changeFilter.SetEnabled(ready)
}

func (u *gui) renderChanges() {
	if u.changingChoices || u.compareLoad != nil || u.changeFilter == nil || u.changeStatus == nil {
		return
	}
	started := time.Now()
	defer func() { logRender(u.ctx, "changes", started, len(u.comparison.Changes), len(u.changePane.model.rows)) }()
	defer u.updateActions()
	diff := u.comparison
	u.enableComparisonControls(diff.Baseline.ID != 0)
	u.changePane.inspectReady = diff.Baseline.Coverage.Subfolders && diff.Latest.Coverage.Subfolders
	if diff.Latest.ID == 0 {
		empty := "Scan this location to start its history."
		if u.active != nil && u.active.scope == u.changeSelection().Label {
			empty = "The first capture is in progress."
		}
		u.changePane.show(nil, "", empty)
		_ = u.driveDelta.SetText("What changed?")
		_ = u.scopeDelta.SetText(" ")
		_ = u.changeStatus.SetText(" ")
		return
	}
	if diff.Baseline.ID == 0 {
		empty := "Capture this location again to compare what changed."
		if u.active != nil && u.active.scope == u.changeSelection().Label {
			empty = "The next capture is in progress."
		}
		u.changePane.show(nil, "", empty)
		_ = u.driveDelta.SetText("First capture saved")
		_ = u.scopeDelta.SetText(timeText(diff.Latest.Captured))
		_ = u.changeStatus.SetText(" ")
		return
	}
	var driveParts []string
	for _, drive := range driveChanges(diff.Baseline.Coverage.Drives, diff.Latest.Coverage.Drives) {
		driveParts = append(driveParts, drive.Root[:2]+"  "+deltaText(drive.UsedDelta, false)+" used  ·  "+sizeText(drive.Free)+" free")
	}
	header := "Actual drive usage   " + strings.Join(driveParts, "     |     ")
	if diff.Latest.Coverage.ScopeKind == "docker" {
		header = "Docker engine and Windows disk changes are separate measurements"
	} else if len(driveParts) == 0 {
		header = "Drive usage comparison unavailable"
	}
	_ = u.driveDelta.SetText(header)
	observed := make(map[string]int64)
	for _, item := range diff.Changes {
		if item.Series == folders {
			observed[volumeRoot(item.item().Path)] += item.Bytes
		}
	}
	var scopeParts []string
	for _, disk := range diff.Latest.Coverage.Drives {
		scopeParts = append(scopeParts, disk.Root[:2]+"  "+deltaText(observed[disk.Root], false))
	}
	scopeText := " "
	if len(scopeParts) > 0 {
		scopeText = "Net change in comparable folders   " + strings.Join(scopeParts, "     |     ")
	}
	_ = u.scopeDelta.SetText(scopeText)
	mode := combo(u.changeMode, folders)
	if mode == subfolders && (!diff.Baseline.Coverage.Subfolders || !diff.Latest.Coverage.Subfolders) {
		u.changePane.show(nil, "", "Choose two captures that include subfolder sizes.")
		_ = u.changeStatus.SetText("New scans include subfolder sizes automatically.")
		return
	}
	direction := combo(u.direction, "Both directions")
	threshold := cutoffBytes[max(0, u.minimum.CurrentIndex())]
	var focused recordFilter
	if u.focused != nil {
		focused.path = normalizedKey(u.focused.path)
	}
	var rows []displayRow
	for _, changed := range diff.Changes {
		if changed.Series != mode || magnitude(changed.Bytes) < threshold ||
			(direction == "Growth" && changed.Bytes < 0) || (direction == "Freed" && changed.Bytes > 0) {
			continue
		}
		item := changed.item()
		if !focused.matches(item, "") {
			continue
		}
		before, after := int64(0), int64(0)
		if changed.Before != nil {
			before = changed.Before.Bytes
		} else if changed.Bounded {
			before = -1
		}
		if changed.After != nil {
			after = changed.After.Bytes
		} else if changed.Bounded {
			after = -1
		}
		amount := deltaText(changed.Bytes, changed.Bounded)
		title := item.Name
		if item.Path != "" {
			title = item.Path
		}
		detail := title + "\r\n\r\n" + amount + "  ·  " + item.Kind +
			"\r\nBefore  " + historySize(before) + "\r\nAfter     " + historySize(after)
		if changed.Before != nil && changed.After != nil && changed.Before.Path != changed.After.Path {
			detail += "\r\nPrevious path  " + changed.Before.Path
		}
		if changed.Bounded {
			detail += "\r\nThis entry was absent or below 64 MiB in one capture."
		} else if item.Detail != "" {
			detail += "\r\n\r\n" + item.Detail
		}
		rows = append(rows, displayRow{Item: item, Rank: magnitude(changed.Bytes), Before: before, After: after, Detail: detail,
			Cells: [4]string{item.Name, amount, historySize(before), historySize(after)}})
	}
	u.changePane.show(rows, u.changeFilter.Text(), "No changes of "+sizeText(threshold)+" or more in this view.")
	u.showChangeSummary(mode, threshold)
}

func (u *gui) searchChanges() {
	if u.changingChoices || u.compareLoad != nil || u.comparison.Baseline.ID == 0 || len(u.changePane.model.items) == 0 {
		return
	}
	u.changePane.search(u.changeFilter, "changes")
	u.showChangeSummary(combo(u.changeMode, folders), cutoffBytes[max(0, u.minimum.CurrentIndex())])
}

func (u *gui) showChangeSummary(mode string, threshold int64) {
	diff := u.comparison
	status := timeText(diff.Baseline.Captured) + " → " + timeText(diff.Latest.Captured) +
		fmt.Sprintf("  ·  %d changes ≥ %s", len(u.changePane.model.rows), sizeText(threshold))
	if diff.ShortWindow {
		status += "  ·  Shorter history available"
	}
	if diff.Omitted > 0 {
		status += fmt.Sprintf("  ·  %d omitted for incomplete coverage", diff.Omitted)
	}
	if mode == images || mode == buildCache {
		status += "  ·  Individual entries can share data"
	}
	_ = u.changeStatus.SetText(status)
}

func logRender(ctx context.Context, view string, started time.Time, input, shown int) {
	elapsed := time.Since(started)
	level := slog.LevelDebug
	if elapsed >= 32*time.Millisecond {
		level = slog.LevelWarn
	}
	logDiagnostic(ctx, level, "gui.render", slog.String("view", view),
		slog.Float64("duration_ms", milliseconds(elapsed)), slog.Int("input_rows", input), slog.Int("visible_rows", shown))
}

func (u *gui) inspectChange() {
	row, ok := u.changePane.selected()
	if !ok || !u.changePane.inspectReady || row.Item.Series != folders || !row.Item.Directory {
		return
	}
	u.focused = &changeFocus{path: row.Item.Path, query: u.changeFilter.Text(), row: rowKey{row.Item.Series, row.Item.Key}}
	_ = u.changePath.SetText("Subfolders in " + row.Item.Path)
	u.changeBreadcrumb.SetVisible(true)
	u.changingChoices = true
	_ = u.changeMode.SetCurrentIndex(slices.Index(supportedSeries("files", true), subfolders))
	_ = u.changeFilter.SetText("")
	u.changingChoices = false
	u.renderChanges()
	_ = u.changePane.table.SetFocus()
}

func (u *gui) clearChangeFocus() {
	if focus := u.focused; focus != nil {
		u.focused = nil
		u.changeBreadcrumb.SetVisible(false)
		u.changingChoices = true
		_ = u.changeFilter.SetText(focus.query)
		u.changingChoices = false
	}
}

func (u *gui) changeModeChanged() {
	if u.changingChoices {
		return
	}
	if combo(u.changeMode, folders) != subfolders {
		u.clearChangeFocus()
	}
	u.renderChanges()
}

func (u *gui) backChanges() {
	if focus := u.focused; focus != nil {
		u.clearChangeFocus()
		_ = u.changeMode.SetCurrentIndex(0)
		u.renderChanges()
		u.changePane.restoreSelection(focus.row)
		_ = u.changePane.table.SetFocus()
	}
}

func historySize(size int64) string {
	if size < 0 {
		return "<64 MiB or absent"
	}
	return sizeText(size)
}
