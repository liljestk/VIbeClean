package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

const pickScans = "Pick two scans"

var (
	periods     = []string{"Previous scan", "Last day", "Last week", "Last month", "Last 90 days", pickScans}
	periodDays  = []int{0, 1, 7, 30, 90}
	cutoffs     = []string{"128 MiB", "256 MiB", "512 MiB", "1 GiB", "5 GiB"}
	cutoffBytes = []int64{128 * units.MiB, model.MinChange, 512 * units.MiB, units.GiB, 5 * units.GiB}
	directions  = []string{"Both directions", "Growth", "Freed"}
)

type changeFocus struct {
	path  string
	name  string
	query string
	row   model.RowKey
}

// metric is one reading in a summary card.
type metric struct {
	label, value, caption string
	// mark shows a removal level's dot before the label.
	mark model.Removal
	// click, when set, makes the reading a shortcut to the items it sums.
	click *widget.Clickable
}

// changesTab holds the ranked differences between two captures.
type changesTab struct {
	changePane      rankedPane
	changeScope     selectBox
	period          selectBox
	fromScan        selectBox
	toScan          selectBox
	changeMode      selectBox
	minimum         selectBox
	direction       selectBox
	changeFilter    searchField
	changeBack      widget.Clickable
	viewFirstScan   widget.Clickable
	scopes          []model.Snapshot
	comparison      service.Comparison
	compareLoad     *guiRead
	historyLoad     *guiRead
	historyScope    string
	history         []model.Snapshot
	historyNotice   string
	scopeLoad       *guiRead
	focused         *changeFocus
	changeStatus    string
	changeNotes     string
	changeDates     string
	changeSummary   []metric
	changeReady     bool
	changingChoices bool
}

func (u *ui) initViews() {
	u.changePane.init([4]column{{title: "Folder"}, {title: "Change", end: true}, {title: "Before", end: true, drop: 2}, {title: "After", end: true, drop: 1}},
		true, "Largest changes")
	u.changePane.ctx = u.ctx
	u.changePane.inspectSeries = model.Folders
	u.changePane.openText, u.changePane.inspectText, u.changePane.inspectIcon = "Open folder", "Inspect folder", iconInspect
	u.changePane.onOpen = func() { u.openSelection(&u.changePane) }
	u.changePane.onInspect = u.inspectChange
	u.changeScope.setOptions([]string{model.QuickScope}, 0)
	u.changeScope.name = "Location to compare"
	u.changeScope.displayValue, u.changeScope.maxWidth, u.changeScope.popupWidth = locationName, 216, 400
	u.period.setOptions(periods, 0)
	u.period.name = "Comparison period"
	u.fromScan.name, u.toScan.name = "Earlier scan", "Later scan"
	u.changeMode.setOptions(model.SupportedSeries(model.ScopeFiles, true), 0)
	u.minimum.setOptions(cutoffs, 1)
	u.minimum.prefix = "≥ "
	u.direction.setOptions(directions, 0)
	u.changeFilter.prompt, u.changeFilter.shortcut = "Search changes", findShortcut
	u.changeStatus = "Run a scan to create a baseline."
	u.initFiles()
	u.initDocker()
	u.initMCP()
}

func (u *ui) changeCaptureText() string {
	selection := u.changeSelection()
	switch {
	case selection.Coverage.ScopeKind == model.ScopeDocker:
		return "Refresh Docker"
	case selection.ID != 0 && selection.Scope == u.comparison.Latest.Scope && u.comparison.Baseline.ID == 0:
		return "Scan again"
	}
	return "Scan now"
}

func (u *ui) captureChanges() {
	selection := u.changeSelection()
	if selection.Coverage.ScopeKind == model.ScopeDocker {
		u.startDocker()
	} else {
		u.startFiles(selection.Label, selection.Coverage.Kind, false)
	}
}

func (u *ui) canOpenFirstScan() bool {
	saved := u.comparison.Latest
	return u.backend != nil && saved.ID != 0 && u.comparison.Baseline.ID == 0 &&
		saved.Coverage.ScopeKind == model.ScopeFiles && saved.Scope == u.changeSelection().Scope &&
		(!u.fileCaptureActive() || strings.EqualFold(saved.Label, u.active.scope))
}

func (u *ui) openFirstScan() {
	saved := u.comparison.Latest
	u.setFileScope(saved.Label)
	u.fileMode.setOptions(u.fileMode.options, 0)
	u.fileRemoval.setOptions(u.fileRemoval.options, 0)
	u.fileFilter.setText("")
	if u.files.ID == saved.ID && u.files.Scope == saved.Scope {
		cancelGUIRead(&u.fileLoad)
		u.restoreFileBrowse(fileBrowseState{sort: 1})
		u.renderFiles()
	} else {
		u.loadFileScope(saved.Label, saved.Scope)
	}
	u.setView(viewFiles)
}

func (u *ui) applyScopes(list []model.Snapshot) {
	previous := u.changeSelection()
	if !slices.ContainsFunc(list, func(s model.Snapshot) bool { return s.Label == model.QuickScope }) {
		list = append(list, model.Snapshot{Label: model.QuickScope, Coverage: model.Coverage{ScopeKind: model.ScopeFiles}})
	}
	u.scopes = list
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
	u.changeScope.setOptions(names, index)
	u.setFileScope(u.fileScopeValue())
	u.chooseChangeScope()
}

func (u *ui) changeSelection() model.Snapshot {
	if index := u.changeScope.index; index >= 0 && index < len(u.scopes) {
		return u.scopes[index]
	}
	return model.Snapshot{Label: model.QuickScope, Coverage: model.Coverage{ScopeKind: model.ScopeFiles}}
}

func (u *ui) chooseChangeScope() {
	selection := u.changeSelection()
	if u.focused != nil && u.comparison.Latest.Scope != selection.Scope {
		u.clearChangeFocus()
	}
	options := model.SupportedSeries(selection.Coverage.ScopeKind, true)
	if !equalStrings(u.changeMode.options, options) {
		selected := slices.Index(options, u.changeMode.value())
		u.changeMode.setOptions(options, max(0, selected))
	}
	u.refreshComparison()
}

func (u *ui) refreshComparison() {
	if u.period.value() == pickScans {
		u.loadChangeHistory()
		return
	}
	cancelGUIRead(&u.historyLoad)
	u.historyNotice = ""
	u.loadComparison()
}

func (u *ui) scanPair() (from, to int64) {
	later := u.toScan.index
	earlier := later + 1 + u.fromScan.index
	if later >= 0 && earlier > later && earlier < len(u.history) {
		return u.history[earlier].ID, u.history[later].ID
	}
	return 0, 0
}

func scanChoices(history []model.Snapshot, chosen int64) ([]string, int) {
	names := make([]string, 0, len(history))
	index := 0
	for i, scan := range history {
		stamp := time.UnixMilli(scan.Captured).Format("2 Jan 2006 15:04:05")
		names = append(names, fmt.Sprintf("%s · #%d", stamp, scan.ID))
		if scan.ID == chosen {
			index = i
		}
	}
	return names, index
}

func (u *ui) loadChangeHistory() {
	backend, scope := u.backend, u.changeSelection().Scope
	cancelGUIRead(&u.compareLoad)
	if u.historyScope != scope {
		u.history = nil
		u.historyScope = scope
		u.fromScan.setOptions(nil, 0)
		u.toScan.setOptions(nil, 0)
		u.comparison = service.Comparison{}
		u.renderChanges()
	}
	if backend == nil || scope == "" {
		u.historyNotice = "Save two scans of this location to choose a pair."
		u.loadComparison()
		return
	}
	from, to := u.scanPair()
	u.historyNotice = "Loading saved scans…"
	readForGUI(u, &u.historyLoad, "gui.compare_history", func(ctx context.Context) ([]model.Snapshot, error) {
		return backend.History(ctx, scope)
	}, func(history []model.Snapshot, err error) {
		u.history = history
		switch {
		case err != nil:
			u.historyNotice = "Could not list saved scans. " + err.Error()
		case len(history) < 2:
			u.historyNotice = "Save two scans of this location to choose a pair."
		default:
			u.historyNotice = ""
		}
		u.toScan.setOptions(scanChoices(history[:max(0, len(history)-1)], to))
		u.chooseEarlierScan(from)
		u.loadComparison()
	})
}

func (u *ui) chooseEarlierScan(from int64) {
	start := min(len(u.history), u.toScan.index+1)
	u.fromScan.setOptions(scanChoices(u.history[start:], from))
}

func (u *ui) loadComparison() {
	window := u.period.index
	if window < 0 {
		return
	}
	scope := u.changeSelection().Scope
	query := service.CompareQuery{Scope: scope}
	if u.period.value() == pickScans {
		query.FromID, query.ToID = u.scanPair()
	} else {
		query.Days = periodDays[window]
	}
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
		u.changeReady = false
	}
	u.changeStatus = "Comparing saved captures…"
	readForGUI(u, &u.compareLoad, "gui.compare", func(ctx context.Context) (service.Comparison, error) {
		return backend.Compare(ctx, query)
	}, func(data service.Comparison, err error) {
		if err != nil {
			if query.FromID != 0 {
				u.comparison = service.Comparison{}
			}
			u.renderChanges()
			if query.FromID != 0 || !errors.Is(err, model.ErrNotFound) {
				u.changeStatus = "Could not load this comparison. " + err.Error()
			}
			return
		}
		u.comparison = data
		u.renderChanges()
	})
}

func (u *ui) captureInProgressHere() bool {
	return u.active != nil && u.active.scope == u.changeSelection().Label
}

func (u *ui) renderChanges() {
	if u.compareLoad != nil {
		return
	}
	started := time.Now()
	defer func() { logRender(u.ctx, "changes", started, len(u.comparison.Changes), len(u.changePane.rows)) }()
	diff := u.comparison
	ready := diff.Baseline.ID != 0
	u.period.disabled = diff.Latest.ID == 0 && u.changeSelection().ID == 0 && u.period.value() != pickScans
	for _, box := range []*selectBox{&u.changeMode, &u.minimum, &u.direction} {
		box.disabled = !ready
	}
	u.changePane.inspectReady = diff.Baseline.Coverage.Subfolders && diff.Latest.Coverage.Subfolders
	u.changeNotes, u.changeDates = "", ""
	switch {
	case diff.Latest.ID == 0:
		empty := "Scan this location to start its history."
		if u.captureInProgressHere() {
			empty = "The first capture is in progress."
		}
		u.showChangeEmpty(empty, "")
		return
	case diff.Baseline.ID == 0:
		empty := "Capture this location again to compare what changed."
		if u.captureInProgressHere() {
			empty = "The next capture is in progress."
		}
		u.showChangeEmpty(empty, "First capture saved "+timeText(diff.Latest.Captured))
		return
	}
	u.changeReady = diff.Latest.Coverage.ScopeKind != model.ScopeDocker
	u.changeSummary = changeMetrics(diff)
	mode := u.changeMode.value()
	if mode == "" {
		mode = model.Folders
	}
	u.changePane.columns[0].title = seriesNoun(mode)
	if mode == model.Subfolders && (!diff.Baseline.Coverage.Subfolders || !diff.Latest.Coverage.Subfolders) {
		u.changePane.show(nil, "", "Choose two captures that include subfolder sizes. New scans include them automatically.")
		u.changeDates = timeText(diff.Baseline.Captured) + " → " + timeText(diff.Latest.Captured)
		return
	}
	if (mode == model.Candidates || mode == model.Labeled) && diff.Baseline.Coverage.Labels != diff.Latest.Coverage.Labels {
		u.changePane.show(nil, "", "Choose two captures made with the same labeling rules.")
		u.changeDates = timeText(diff.Baseline.Captured) + " → " + timeText(diff.Latest.Captured)
		return
	}
	threshold := cutoffBytes[max(0, u.minimum.index)]
	rows := u.changeRows(diff, mode, threshold)
	u.changePane.show(rows, u.changeFilter.text(), "No changes of "+units.Size(threshold)+" or more in this view.")
	u.showChangeSummary(mode)
}

func (u *ui) showChangeEmpty(empty, status string) {
	u.changePane.show(nil, "", empty)
	u.changeReady = false
	u.changeStatus = status
	u.changeDates = status
}

// changeMetrics sums the drive movement and comparable folders across drives.
func changeMetrics(diff service.Comparison) []metric {
	var used, free int64
	var usedParts, freeParts []string
	for _, drive := range diff.Drives {
		used += drive.UsedDelta
		free += drive.Free
		usedParts = append(usedParts, driveName(drive.Root)+" "+units.Delta(drive.UsedDelta, false))
		freeParts = append(freeParts, driveName(drive.Root)+" "+units.Size(drive.Free))
	}
	observed := make(map[string]int64)
	var folders int64
	for _, item := range diff.Changes {
		if item.Series == model.Folders {
			observed[model.VolumeRoot(item.Item().Path)] += item.Bytes
			folders += item.Bytes
		}
	}
	var folderParts []string
	for _, disk := range diff.Latest.Coverage.Drives {
		folderParts = append(folderParts, driveName(disk.Root)+" "+units.Delta(observed[disk.Root], false))
	}
	caption := func(parts []string) string {
		if len(parts) < 2 {
			return ""
		}
		return strings.Join(parts, "  ·  ")
	}
	usedText, freeText := "Unavailable", "Unavailable"
	if len(diff.Drives) > 0 {
		usedText, freeText = units.Delta(used, false), units.Size(free)
	}
	return []metric{
		{label: "Drive usage change", value: usedText, caption: caption(usedParts)},
		{label: "Tracked folder change", value: units.Delta(folders, false), caption: caption(folderParts)},
		{label: "Free at later scan", value: freeText, caption: caption(freeParts)},
	}
}

func (u *ui) changeRows(diff service.Comparison, mode string, threshold int64) []displayRow {
	direction := u.direction.value()
	var focused model.Filter
	if u.focused != nil {
		focused = model.FolderFilter(u.focused.path)
	}
	files := diff.Latest.Coverage.ScopeKind == model.ScopeFiles
	var rows []displayRow
	for _, changed := range diff.Changes {
		if changed.Series != mode || units.Magnitude(changed.Bytes) < threshold ||
			(direction == directions[1] && changed.Bytes < 0) || (direction == directions[2] && changed.Bytes > 0) {
			continue
		}
		if item := changed.Item(); focused.Matches(item, "") {
			row := changeRow(changed)
			if files && item.Kind != "" {
				labels := diff.Latest.Coverage.Labels
				if changed.After == nil {
					labels = diff.Baseline.Coverage.Labels
				}
				row.Mark = model.CaptureRemoval(item.Kind, labels)
				row.Facts = append(row.Facts, fact{"Removal", removalTitle(row.Mark)})
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func changeRow(changed model.Change) displayRow {
	item := changed.Item()
	before, after := sideSize(changed.Before, changed.Bounded), sideSize(changed.After, changed.Bounded)
	amount := units.Delta(changed.Bytes, changed.Bounded)
	var notes []string
	if changed.Before != nil && changed.After != nil && changed.Before.Path != changed.After.Path {
		notes = append(notes, "Previously at "+changed.Before.Path)
	}
	if changed.Bounded {
		notes = append(notes, "This entry was absent or below 64 MiB in one capture, so the change is a lower bound.")
	} else if item.Detail != "" {
		notes = append(notes, item.Detail)
	}
	facts := []fact{{"Before", historySize(before)}, {"After", historySize(after)}}
	if item.Kind != "" {
		facts = append(facts, fact{"Category", item.Kind})
	}
	return displayRow{Item: item, Rank: units.Magnitude(changed.Bytes), Delta: changed.Bytes, Before: before, After: after,
		Detail: strings.Join(notes, "\n"), Facts: facts,
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

func historySize(size int64) string {
	if size < 0 {
		return "<64 MiB or absent"
	}
	return units.Size(size)
}

func (u *ui) searchChanges() {
	if u.compareLoad != nil || u.comparison.Baseline.ID == 0 || len(u.changePane.items) == 0 {
		return
	}
	u.changePane.search(u.changeFilter.text(), "changes")
	u.showChangeSummary(u.changeMode.value())
}

func (u *ui) showChangeSummary(mode string) {
	diff := u.comparison
	u.changeDates = timeText(diff.Baseline.Captured) + " → " + timeText(diff.Latest.Captured)
	var notes []string
	if diff.ShortWindow {
		notes = append(notes, "Only the dates above are available for this period")
	}
	if diff.Omitted > 0 {
		notes = append(notes, fmt.Sprintf("%d readings could not be compared", diff.Omitted))
	}
	if diff.Baseline.Coverage.Estimated > 0 || diff.Latest.Coverage.Estimated > 0 {
		notes = append(notes, "Some locations used estimated sizes")
	}
	if mode == model.Images || mode == model.BuildCache {
		notes = append(notes, "Individual entries can share data")
	}
	if diff.Latest.Coverage.ScopeKind == model.ScopeDocker {
		notes = append(notes, "Engine and Windows disk are measured separately")
	}
	if mode == model.LargeFiles || mode == model.Subfolders {
		notes = append(notes, "Large files and subfolders are tracked from 64 MiB")
	}
	u.changeNotes = strings.Join(notes, "  ·  ")
	u.changeStatus = ""
}

func (u *ui) inspectChange() {
	row, ok := u.changePane.selected()
	if !ok || !u.changePane.inspectReady || row.Item.Series != model.Folders || !row.Item.Directory {
		return
	}
	u.focused = &changeFocus{path: row.Item.Path, name: row.Item.Name, query: u.changeFilter.text(),
		row: model.RowKey{Series: row.Item.Series, Key: row.Item.Key}}
	u.changeMode.setOptions(u.changeMode.options, slices.Index(u.changeMode.options, model.Subfolders))
	u.changeFilter.setText("")
	u.renderChanges()
}

func (u *ui) clearChangeFocus() {
	if focus := u.focused; focus != nil {
		u.focused = nil
		u.changeFilter.setText(focus.query)
	}
}

func (u *ui) backChanges() {
	if focus := u.focused; focus != nil {
		u.clearChangeFocus()
		u.changeMode.setOptions(u.changeMode.options, 0)
		u.renderChanges()
		u.changePane.restoreSelection(focus.row)
	}
}

func (u *ui) changeListTitle() string {
	if u.focused != nil {
		return "Subfolders in " + u.focused.name
	}
	switch mode := u.changeMode.value(); mode {
	case model.Folders:
		return "Folder changes"
	case model.Subfolders:
		return "Subfolder changes"
	case model.Candidates:
		return "Review candidate changes"
	case model.Labeled:
		return "App, game and project changes"
	case model.LargeFiles:
		return "Large file changes"
	case "":
		return "Changes"
	default:
		return mode + " changes"
	}
}

// changesView lays out the context controls, the summary and the results.
func (u *ui) changesView(gtx layout.Context) layout.Dimensions {
	t := u.th
	if u.changeScope.Changed() {
		u.chooseChangeScope()
	}
	if u.period.Changed() {
		u.refreshComparison()
	}
	if u.toScan.Changed() {
		u.chooseEarlierScan(0)
		u.loadComparison()
	}
	if u.fromScan.Changed() {
		u.loadComparison()
	}
	if u.changeMode.Changed() {
		if u.changeMode.value() != model.Subfolders {
			u.clearChangeFocus()
		}
		u.renderChanges()
	}
	if u.minimum.Changed() || u.direction.Changed() {
		u.renderChanges()
	}
	if u.changeBack.Clicked(gtx) {
		u.backChanges()
	}
	context := func(gtx layout.Context) layout.Dimensions {
		controls := []layout.Widget{
			func(gtx layout.Context) layout.Dimensions {
				return labeled(gtx, t, "Location", func(gtx layout.Context) layout.Dimensions { return u.changeScope.Layout(gtx, t) })
			},
			func(gtx layout.Context) layout.Dimensions {
				return labeled(gtx, t, "Compare", func(gtx layout.Context) layout.Dimensions { return u.period.Layout(gtx, t) })
			},
		}
		if u.period.value() == pickScans && len(u.history) >= 2 {
			controls = append(controls,
				func(gtx layout.Context) layout.Dimensions {
					return labeled(gtx, t, "From", func(gtx layout.Context) layout.Dimensions { return u.fromScan.Layout(gtx, t) })
				},
				func(gtx layout.Context) layout.Dimensions {
					return labeled(gtx, t, "To", func(gtx layout.Context) layout.Dimensions { return u.toScan.Layout(gtx, t) })
				},
			)
		}
		return flow{gap: 16, rowGap: 8, middle: true}.Layout(gtx, controls...)
	}
	children := []layout.FlexChild{layout.Rigid(context)}
	if dateLine := strings.TrimSpace(strings.Join([]string{u.changeDates, u.historyNotice}, "  ")); dateLine != "" {
		children = append(children, layout.Rigid(space(0, 6)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.small(t.quiet).wrapped(2), dateLine)
		}))
	}
	if u.canOpenFirstScan() {
		children = append(children, layout.Rigid(space(0, 8)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = 0
			return t.button(gtx, &u.viewFirstScan, buttonStyle{icon: iconFolder, label: "View what uses space"})
		}))
	}
	children = append(children, layout.Rigid(space(0, 14)))
	if u.changeReady {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.summaryCard(gtx, u.changeSummary) }),
			layout.Rigid(space(0, 16)))
	}
	count := len(u.changePane.rows)
	children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
		return u.results(gtx, &u.changePane, resultsHeader{
			title: u.changeListTitle(),
			count: itemCount(count, u.comparison.Baseline.ID != 0),
			notes: strings.TrimSpace(strings.Join([]string{u.changeNotes, u.changeStatus}, "  ")),
			back: func() *widget.Clickable {
				if u.focused != nil {
					return &u.changeBack
				}
				return nil
			}(),
			backText: "Back to folders",
			search: func(gtx layout.Context) layout.Dimensions {
				d, changed := u.changeFilter.Layout(gtx, t, u.comparison.Baseline.ID == 0)
				if changed {
					u.searchChanges()
				}
				return d
			},
			filters: []layout.Widget{
				func(gtx layout.Context) layout.Dimensions { return u.changeMode.Layout(gtx, t) },
				func(gtx layout.Context) layout.Dimensions { return u.direction.Layout(gtx, t) },
				func(gtx layout.Context) layout.Dimensions { return u.minimum.Layout(gtx, t) },
			},
		})
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func itemCount(n int, known bool) string {
	switch {
	case !known:
		return ""
	case n == 1:
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}

// seriesNoun names the first column for a series.
func seriesNoun(series string) string {
	switch series {
	case model.Folders, model.Subfolders, model.Candidates, model.Labeled:
		return "Folder"
	case model.Categories:
		return "Category"
	case model.LargeFiles:
		return "File"
	case model.Projects:
		return "Project"
	case model.DockerTotals:
		return "Total"
	}
	return "Resource"
}
