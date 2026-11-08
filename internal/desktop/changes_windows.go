package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

var (
	periods     = []string{"Previous scan", "Last day", "Last week", "Last month", "Last 90 days"}
	periodDays  = []int{0, 1, 7, 30, 90}
	cutoffs     = []string{"128 MiB", "256 MiB", "512 MiB", "1 GiB", "5 GiB"}
	cutoffBytes = []int64{128 * units.MiB, model.MinChange, 512 * units.MiB, units.GiB, 5 * units.GiB}
	directions  = []string{"Both directions", "Growth", "Freed"}
)

const columnSeparator = "     |     "

type changeFocus struct {
	path  string
	query string
	row   model.RowKey
}

// changesTab holds the Changes tab: ranked differences between two captures.
type changesTab struct {
	changePane       rankedPane
	changeScope      *walk.ComboBox
	changeMode       *walk.ComboBox
	period           *walk.ComboBox
	minimum          *walk.ComboBox
	direction        *walk.ComboBox
	changeFilter     *walk.LineEdit
	changeStatus     *walk.Label
	driveDelta       *walk.Label
	scopeDelta       *walk.Label
	scopes           []model.Snapshot
	changingChoices  bool
	comparison       service.Comparison
	compareLoad      *guiRead
	scopeLoad        *guiRead
	focused          *changeFocus
	changePath       *walk.Label
	changeBreadcrumb *walk.Composite
	changeCapture    *walk.PushButton
}

func (u *gui) changesPage() d.TabPage {
	u.changePane.inspectSeries = model.Folders
	return d.TabPage{Title: "Changes", Layout: d.VBox{}, Children: []d.Widget{
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Location"},
			d.ComboBox{AssignTo: &u.changeScope, Model: []string{model.QuickScope}, CurrentIndex: 0,
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
			d.ComboBox{AssignTo: &u.changeMode, Model: model.SupportedSeries(model.ScopeFiles, true), CurrentIndex: 0,
				MinSize: d.Size{Width: 155}, OnCurrentIndexChanged: u.changeModeChanged,
				Accessibility: d.Accessibility{Name: "Result type"}},
			d.Label{Text: "Minimum"},
			d.ComboBox{AssignTo: &u.minimum, Model: cutoffs, CurrentIndex: 1,
				MinSize: d.Size{Width: 82}, OnCurrentIndexChanged: u.renderChanges,
				Accessibility: d.Accessibility{Name: "Minimum change"}},
			d.ComboBox{AssignTo: &u.direction, Model: directions, CurrentIndex: 0,
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

func (u *gui) changeCaptureText() string {
	selection := u.changeSelection()
	switch {
	case selection.Coverage.ScopeKind == model.ScopeDocker:
		return "Refresh Docker"
	case selection.ID != 0 && selection.Scope == u.comparison.Latest.Scope && u.comparison.Baseline.ID == 0:
		return "Scan again"
	}
	return "Scan now"
}

func (u *gui) captureChanges() {
	selection := u.changeSelection()
	if selection.Coverage.ScopeKind == model.ScopeDocker {
		u.startDocker()
	} else {
		u.startFiles(selection.Label, selection.Coverage.Kind, false)
	}
}

func (u *gui) applyScopes(list []model.Snapshot) {
	previous := u.changeSelection()
	if !slices.ContainsFunc(list, func(s model.Snapshot) bool { return s.Label == model.QuickScope }) {
		list = append(list, model.Snapshot{Label: model.QuickScope, Coverage: model.Coverage{ScopeKind: model.ScopeFiles}})
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
	if current, _ := u.changeScope.Model().([]string); !slices.Equal(current, names) {
		_ = u.changeScope.SetModel(names)
	}
	if index != u.changeScope.CurrentIndex() {
		_ = u.changeScope.SetCurrentIndex(index)
	}
	u.changingChoices = false
	u.setFileScope(combo(u.fileScope, model.QuickScope))
	u.chooseChangeScope()
}

func (u *gui) changeSelection() model.Snapshot {
	if u.changeScope != nil {
		if index := u.changeScope.CurrentIndex(); index >= 0 && index < len(u.scopes) {
			return u.scopes[index]
		}
	}
	return model.Snapshot{Label: model.QuickScope, Coverage: model.Coverage{ScopeKind: model.ScopeFiles}}
}

func (u *gui) chooseChangeScope() {
	if u.changingChoices || u.changeMode == nil || u.changeScope == nil {
		return
	}
	selection := u.changeSelection()
	if u.focused != nil && u.comparison.Latest.Scope != selection.Scope {
		u.clearChangeFocus()
	}
	options := model.SupportedSeries(selection.Coverage.ScopeKind, true)
	if current, _ := u.changeMode.Model().([]string); !slices.Equal(current, options) {
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
	backend := u.backend
	if scope == "" || backend == nil {
		cancelGUIRead(&u.compareLoad)
		u.comparison = service.Comparison{}
		u.renderChanges()
		return
	}
	if u.comparison.Latest.Scope != scope {
		u.comparison = service.Comparison{}
		u.changePane.show(nil, "", "Loading saved history…")
		_ = u.driveDelta.SetText("What changed?")
		_ = u.scopeDelta.SetText(" ")
	}
	_ = u.changeStatus.SetText("Comparing saved captures…")
	readForGUI(u, &u.compareLoad, "gui.compare", func(ctx context.Context) (service.Comparison, error) {
		return backend.Compare(ctx, service.CompareQuery{Scope: scope, Days: days})
	}, func(data service.Comparison, err error) {
		if err != nil {
			u.renderChanges()
			if !errors.Is(err, model.ErrNotFound) {
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

func (u *gui) captureInProgressHere() bool {
	return u.active != nil && u.active.scope == u.changeSelection().Label
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
	switch {
	case diff.Latest.ID == 0:
		empty := "Scan this location to start its history."
		if u.captureInProgressHere() {
			empty = "The first capture is in progress."
		}
		u.showChangeHeadline(empty, "What changed?", " ")
		return
	case diff.Baseline.ID == 0:
		empty := "Capture this location again to compare what changed."
		if u.captureInProgressHere() {
			empty = "The next capture is in progress."
		}
		u.showChangeHeadline(empty, "First capture saved", timeText(diff.Latest.Captured))
		return
	}
	_ = u.driveDelta.SetText(driveHeader(diff))
	_ = u.scopeDelta.SetText(scopeSummary(diff))
	mode := combo(u.changeMode, model.Folders)
	if mode == model.Subfolders && (!diff.Baseline.Coverage.Subfolders || !diff.Latest.Coverage.Subfolders) {
		u.changePane.show(nil, "", "Choose two captures that include subfolder sizes.")
		_ = u.changeStatus.SetText("New scans include subfolder sizes automatically.")
		return
	}
	threshold := cutoffBytes[max(0, u.minimum.CurrentIndex())]
	rows := u.changeRows(diff, mode, threshold)
	u.changePane.show(rows, u.changeFilter.Text(), "No changes of "+units.Size(threshold)+" or more in this view.")
	u.showChangeSummary(mode, threshold)
}

func (u *gui) showChangeHeadline(empty, headline, detail string) {
	u.changePane.show(nil, "", empty)
	_ = u.driveDelta.SetText(headline)
	_ = u.scopeDelta.SetText(detail)
	_ = u.changeStatus.SetText(" ")
}

func driveHeader(diff service.Comparison) string {
	if diff.Latest.Coverage.ScopeKind == model.ScopeDocker {
		return "Docker engine and Windows disk changes are separate measurements"
	}
	var parts []string
	for _, drive := range diff.Drives {
		parts = append(parts, drive.Root[:2]+"  "+units.Delta(drive.UsedDelta, false)+" used  ·  "+units.Size(drive.Free)+" free")
	}
	if len(parts) == 0 {
		return "Drive usage comparison unavailable"
	}
	return "Actual drive usage   " + strings.Join(parts, columnSeparator)
}

// scopeSummary sums the folder changes on each drive.
func scopeSummary(diff service.Comparison) string {
	observed := make(map[string]int64)
	for _, item := range diff.Changes {
		if item.Series == model.Folders {
			observed[model.VolumeRoot(item.Item().Path)] += item.Bytes
		}
	}
	var parts []string
	for _, disk := range diff.Latest.Coverage.Drives {
		parts = append(parts, disk.Root[:2]+"  "+units.Delta(observed[disk.Root], false))
	}
	if len(parts) == 0 {
		return " "
	}
	return "Net change in comparable folders   " + strings.Join(parts, columnSeparator)
}

func (u *gui) changeRows(diff service.Comparison, mode string, threshold int64) []displayRow {
	direction := combo(u.direction, directions[0])
	var focused model.Filter
	if u.focused != nil {
		focused = model.FolderFilter(u.focused.path)
	}
	var rows []displayRow
	for _, changed := range diff.Changes {
		if changed.Series != mode || units.Magnitude(changed.Bytes) < threshold ||
			(direction == directions[1] && changed.Bytes < 0) || (direction == directions[2] && changed.Bytes > 0) {
			continue
		}
		if item := changed.Item(); focused.Matches(item, "") {
			rows = append(rows, changeRow(changed))
		}
	}
	return rows
}

func changeRow(changed model.Change) displayRow {
	item := changed.Item()
	before, after := sideSize(changed.Before, changed.Bounded), sideSize(changed.After, changed.Bounded)
	amount := units.Delta(changed.Bytes, changed.Bounded)
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
	return displayRow{Item: item, Rank: units.Magnitude(changed.Bytes), Before: before, After: after, Detail: detail,
		Cells: [4]string{item.Name, amount, historySize(before), historySize(after)}}
}

// sideSize is a side's size, or -1 when a bounded change has no reading there.
func sideSize(side *model.Record, bounded bool) int64 {
	switch {
	case side != nil:
		return side.Bytes
	case bounded:
		return -1
	}
	return 0
}

func (u *gui) searchChanges() {
	if u.changingChoices || u.compareLoad != nil || u.comparison.Baseline.ID == 0 || len(u.changePane.model.items) == 0 {
		return
	}
	u.changePane.search(u.changeFilter, "changes")
	u.showChangeSummary(combo(u.changeMode, model.Folders), cutoffBytes[max(0, u.minimum.CurrentIndex())])
}

func (u *gui) showChangeSummary(mode string, threshold int64) {
	diff := u.comparison
	status := timeText(diff.Baseline.Captured) + " → " + timeText(diff.Latest.Captured) +
		fmt.Sprintf("  ·  %d changes ≥ %s", len(u.changePane.model.rows), units.Size(threshold))
	if diff.ShortWindow {
		status += "  ·  Shorter history available"
	}
	if diff.Omitted > 0 {
		status += fmt.Sprintf("  ·  %d omitted for incomplete coverage", diff.Omitted)
	}
	if mode == model.Images || mode == model.BuildCache {
		status += "  ·  Individual entries can share data"
	}
	_ = u.changeStatus.SetText(status)
}

func (u *gui) inspectChange() {
	row, ok := u.changePane.selected()
	if !ok || !u.changePane.inspectReady || row.Item.Series != model.Folders || !row.Item.Directory {
		return
	}
	u.focused = &changeFocus{path: row.Item.Path, query: u.changeFilter.Text(), row: model.RowKey{Series: row.Item.Series, Key: row.Item.Key}}
	_ = u.changePath.SetText("Subfolders in " + row.Item.Path)
	u.changeBreadcrumb.SetVisible(true)
	u.changingChoices = true
	_ = u.changeMode.SetCurrentIndex(slices.Index(model.SupportedSeries(model.ScopeFiles, true), model.Subfolders))
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
	if combo(u.changeMode, model.Folders) != model.Subfolders {
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
	return units.Size(size)
}
