package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

type fileViewState struct {
	saved   model.Snapshot
	mode    string
	removal int
	query   string
	row     model.RowKey
}

// anyRemoval is the removal filter option that shows every item.
const anyRemoval = "All items"

// removalOptions lists the removal filter: every item, then each level.
func removalOptions() []string {
	options := []string{anyRemoval}
	for _, r := range model.Removals {
		options = append(options, string(r))
	}
	return options
}

// filesTab holds the Files view: the latest capture of one location.
type filesTab struct {
	filePane    rankedPane
	files       model.Snapshot
	back        []fileViewState
	fileScope   selectBox
	fileMode    selectBox
	fileRemoval selectBox
	fileFilter  searchField
	// removalLinks are the summary shortcuts to safe, checked and uninstallable items.
	removalLinks [3]widget.Clickable
	browse       widget.Clickable
	backButton   widget.Clickable
	fileLoad     *guiRead
	fileNotes    string
	fileSummary  []metric
	choosing     bool
}

func (u *ui) initFiles() {
	u.filePane.init([4]column{{title: "Folder"}, {title: "On disk", end: true}, {title: "Category", drop: 2}, {title: "Modified", end: true, drop: 1}},
		false, "Largest items")
	u.filePane.marked = 2
	u.filePane.ctx = u.ctx
	u.filePane.openText, u.filePane.inspectText, u.filePane.inspectIcon = "Open folder", "Inspect folder", iconInspect
	u.filePane.onOpen = func() { u.openSelection(&u.filePane) }
	u.filePane.onInspect = u.inspectFolder
	u.fileScope.setOptions([]string{model.QuickScope}, 0)
	u.fileMode.setOptions(model.SupportedSeries(model.ScopeFiles, false), 0)
	u.fileRemoval.setOptions(removalOptions(), 0)
	u.fileFilter.prompt = "Search files"
	u.files = model.Snapshot{Label: model.QuickScope}
}

func (u *ui) fileScopeValue() string {
	if value := u.fileScope.value(); value != "" {
		return value
	}
	return model.QuickScope
}

// setFileScope lists Quick review, the fixed drives and saved folders, and selects scope.
func (u *ui) setFileScope(scope string) {
	options := append([]string{model.QuickScope}, u.driveRoots...)
	for _, saved := range u.scopes {
		if saved.Coverage.ScopeKind == model.ScopeFiles && model.VolumeRoot(saved.Label) != "" && !slices.Contains(options, saved.Label) {
			options = append(options, saved.Label)
		}
	}
	if !slices.Contains(options, scope) {
		options = append(options, scope)
	}
	u.fileScope.setOptions(options, slices.Index(options, scope))
}

func (u *ui) selectFileScope() {
	scope := u.fileScopeValue()
	if strings.EqualFold(scope, u.files.Label) {
		return
	}
	u.loadFileScope(scope)
}

func (u *ui) loadFileScope(scope string) {
	backend := u.backend
	if backend == nil {
		return
	}
	u.files = model.Snapshot{Label: scope}
	readForGUI(u, &u.fileLoad, "gui.file_location", func(ctx context.Context) (model.Snapshot, error) {
		return latestSnapshot(ctx, backend, scope)
	}, func(saved model.Snapshot, err error) {
		if err == nil {
			u.files = saved
		}
		u.renderFiles()
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			u.setActivity("Could not load the saved scan. "+err.Error(), toneProblem)
		}
	})
	u.renderFiles()
}

func (u *ui) scanClick() {
	scope := u.fileScopeValue()
	kind := ""
	if strings.EqualFold(scope, u.files.Label) {
		kind = u.files.Coverage.Kind
	}
	u.startFiles(scope, kind, false)
}

// startFiles captures a location. remember keeps the current view for Back.
func (u *ui) startFiles(scope, kind string, remember bool) {
	backend := u.backend
	if backend == nil || u.active != nil {
		return
	}
	if scope != model.QuickScope {
		scope = filepath.Clean(scope)
	}
	if remember && u.files.Label != "" && !strings.EqualFold(scope, u.files.Label) {
		u.rememberView()
	}
	u.setFileScope(scope)
	if !strings.EqualFold(scope, u.files.Label) {
		u.loadFileScope(scope)
	}
	if remember {
		u.fileMode.setOptions(u.fileMode.options, 0)
		u.fileRemoval.setOptions(u.fileRemoval.options, 0)
		u.fileFilter.setText("")
	}
	ctx := u.beginCapture(scope, "Scanning "+scope+"…")
	task := u.active
	u.renderFiles()
	go func() {
		defer diag.RecoverPanic(ctx, "gui.files_worker")
		saved, err := backend.Capture(ctx, service.CaptureRequest{Scope: scope, Kind: kind}, func(p service.Progress) {
			u.post(func() {
				if u.active == task && ctx.Err() == nil {
					task.checked = p.FilesChecked
				}
			})
		})
		if u.ctx.Err() != nil {
			return
		}
		var disks []model.DriveState
		if err == nil {
			disks, _ = backend.Drives(u.ctx)
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

func (u *ui) rememberView() {
	state := fileViewState{saved: u.files, mode: u.fileMode.value(), removal: u.fileRemoval.index, query: u.fileFilter.text()}
	if key, ok := u.filePane.selectedKey(); ok {
		state.row = key
	}
	u.back = append(u.back, state)
	if len(u.back) > historyDepth {
		u.back = u.back[len(u.back)-historyDepth:]
	}
}

func (u *ui) chooseFolder() {
	if u.backend == nil || u.active != nil || u.choosing {
		return
	}
	u.choosing = true
	hwnd := u.hwnd
	go func() {
		path, err := pickFolder(hwnd, "Choose a folder on a local fixed drive")
		u.post(func() {
			u.choosing = false
			switch {
			case err != nil:
				diag.Log(u.ctx, slog.LevelWarn, "gui.folder_dialog_failed", slog.String("error_kind", diag.ErrorKind(err)))
				u.setActivity("Could not open the folder chooser. "+err.Error(), toneProblem)
			case path != "":
				u.startFiles(path, "", true)
			}
		})
	}()
}

func (u *ui) inspectFolder() {
	if row, ok := u.filePane.selected(); ok && row.Item.Directory {
		u.startFiles(row.Item.Path, row.Item.Kind, true)
	}
}

func (u *ui) goBack() {
	if len(u.back) == 0 || u.fileCaptureActive() {
		return
	}
	cancelGUIRead(&u.fileLoad)
	state := u.back[len(u.back)-1]
	u.files = state.saved
	u.back = u.back[:len(u.back)-1]
	u.setFileScope(u.files.Label)
	u.fileMode.setOptions(u.fileMode.options, max(0, slices.Index(u.fileMode.options, state.mode)))
	u.fileRemoval.setOptions(u.fileRemoval.options, state.removal)
	u.fileFilter.setText(state.query)
	u.renderFiles()
	u.filePane.restoreSelection(state.row)
}

// removalFilter returns the chosen removal level, or "" for every item.
func (u *ui) removalFilter() model.Removal {
	if u.fileRemoval.index <= 0 {
		return ""
	}
	return model.Removal(u.fileRemoval.value())
}

// showRemoval lists the items of one removal level, largest first: cleanup
// candidates for levels to clear or check, programs and games to uninstall.
func (u *ui) showRemoval(r model.Removal) {
	series := model.Candidates
	if r == model.Uninstall {
		series = model.Labeled
	}
	u.fileMode.setOptions(u.fileMode.options, max(0, slices.Index(u.fileMode.options, series)))
	u.fileRemoval.setOptions(u.fileRemoval.options, slices.Index(u.fileRemoval.options, string(r)))
	u.fileFilter.setText("")
	u.filePane.sorted = false
	u.renderFiles()
}

func (u *ui) renderFiles() {
	started := time.Now()
	defer func() { logRender(u.ctx, "files", started, len(u.files.Records), len(u.filePane.rows)) }()
	series := u.fileMode.value()
	if series == "" {
		series = model.Folders
	}
	removal := u.removalFilter()
	columns := &u.filePane.columns
	columns[0].title = seriesNoun(series)
	columns[2].title, columns[3].title = "Category", "Modified"
	var rows []displayRow
	if series == model.Categories {
		columns[2].title, columns[3].title = "Removal", "Share"
		rows = categoryRows(u.files, removal)
	} else {
		rows = fileRows(u.files, series, removal)
	}
	u.filePane.show(rows, u.fileFilter.text(), u.filesEmptyText())
	u.fileSummary, u.fileNotes = nil, ""
	if u.files.ID == 0 {
		return
	}
	labeled := u.files.Coverage.Labels > 0
	switch {
	case series == model.Subfolders && !u.files.Coverage.Subfolders:
		u.fileNotes = "Scan this location again to index its subfolders."
	case (series == model.Categories || series == model.Labeled) && !labeled:
		u.fileNotes = "Scan this location again to label its contents."
	case series == model.Categories || series == model.Labeled || removal != "":
		u.fileNotes = "Labels come from where Windows, launchers and tools keep files, and from markers such as a .git folder. " +
			"Check an item before you delete it."
	case u.files.Label == model.QuickScope:
		u.fileNotes = "Quick review captures daily while it is selected and the app is open. File allocation is counted once per scan."
	}
	u.fileSummary = u.fileMetrics()
}

// fileRows prepares the readings of one series, keeping those with the
// chosen removal level.
func fileRows(saved model.Snapshot, series string, removal model.Removal) []displayRow {
	var rows []displayRow
	for _, item := range saved.Records {
		if item.Series != series {
			continue
		}
		var mark model.Removal
		if item.Kind != "" {
			mark = model.RemovalOf(item.Kind)
		}
		if removal != "" && mark != removal {
			continue
		}
		var facts []fact
		if item.Directory {
			facts = append(facts, fact{"Files", thousands(item.Files)})
		}
		if item.Modified > 0 {
			facts = append(facts, fact{"Modified", fileDate(item.Modified)})
		}
		if item.Kind != "" {
			facts = append(facts, fact{"Category", item.Kind}, fact{"Removal", string(mark)})
		}
		detail := model.KindNote(item.Kind)
		if item.Detail != "" {
			detail = item.Detail + " " + detail
		}
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: -1, After: -1, Detail: detail, Facts: facts,
			Mark: mark, Cells: [4]string{item.Name, units.Size(item.Bytes), item.Kind, fileDate(item.Modified)}})
	}
	return rows
}

// categoryRows prepares the category totals with their share of the
// measured space. Removal levels sort from the easiest win.
func categoryRows(saved model.Snapshot, removal model.Removal) []displayRow {
	var total int64
	for _, item := range saved.Records {
		if item.Series == model.Categories {
			total += item.Bytes
		}
	}
	var rows []displayRow
	for _, item := range saved.Records {
		mark := model.RemovalOf(item.Kind)
		if item.Series != model.Categories || (removal != "" && mark != removal) {
			continue
		}
		share := shareText(item.Bytes, total)
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: int64(slices.Index(model.Removals, mark)),
			After: item.Bytes, Detail: model.KindNote(item.Kind) + "\n" + model.RemovalNote(mark), Mark: mark,
			Facts: []fact{{"Files", thousands(item.Files)}, {"Share", share}, {"Removal", string(mark)}},
			Cells: [4]string{item.Kind, units.Size(item.Bytes), string(mark), share}})
	}
	return rows
}

// shareText formats part of a whole as a percentage.
func shareText(part, whole int64) string {
	if whole <= 0 {
		return ""
	}
	percent := float64(part) * 100 / float64(whole)
	if percent < 0.1 {
		return "<0.1%"
	}
	return fmt.Sprintf("%.1f%%", percent)
}

// fileMetrics summarizes the capture. Captures with category totals lead
// with what can be cleared, checked or uninstalled; each of those readings
// opens the matching items.
func (u *ui) fileMetrics() []metric {
	var total, free int64
	for _, item := range u.files.Records {
		if item.Series == model.Folders {
			total += item.Bytes
		}
	}
	for _, disk := range u.files.Coverage.Drives {
		free += disk.Free
	}
	c := u.files.Coverage
	var metrics []metric
	if totals, ok := model.RemovalTotals(u.files); ok {
		checked := thousands(c.Checked) + " files"
		if gaps := len(c.Gaps); gaps > 0 {
			checked += fmt.Sprintf(" · %s unreadable", thousands(int64(gaps)))
		}
		metrics = append(metrics, metric{label: "Measured", value: units.Size(total), caption: checked})
		for i, r := range model.Removals[:len(u.removalLinks)] {
			metrics = append(metrics, metric{label: string(r), value: units.Size(totals[r]),
				caption: largestKinds(u.files, r), mark: r, click: &u.removalLinks[i]})
		}
	} else {
		repeated := ""
		if c.Shared > 0 {
			repeated = fmt.Sprintf("%s repeated links counted once", thousands(c.Shared))
		}
		unreadable := ""
		if len(c.Gaps) > 0 {
			unreadable = "Locations the scan could not read"
		}
		metrics = []metric{
			{label: "Measured", value: units.Size(total), caption: u.files.Label},
			{label: "Files checked", value: thousands(c.Checked), caption: repeated},
			{label: "Unreadable", value: thousands(int64(len(c.Gaps))), caption: unreadable},
		}
	}
	if len(c.Drives) > 0 {
		metrics = append(metrics, metric{label: "Drive space free", value: units.Size(free)})
	}
	return metrics
}

// largestKinds names the two largest categories with a removal level.
func largestKinds(saved model.Snapshot, r model.Removal) string {
	var kinds []model.Record
	for _, item := range saved.Records {
		if item.Series == model.Categories && model.RemovalOf(item.Kind) == r {
			kinds = append(kinds, item)
		}
	}
	model.SortRecords(kinds)
	var names []string
	for _, item := range kinds[:min(2, len(kinds))] {
		names = append(names, item.Kind)
	}
	return strings.Join(names, ", ")
}

func (u *ui) filesEmptyText() string {
	switch {
	case u.files.ID != 0:
		return "No items in this view."
	case u.fileCaptureActive():
		return "Measuring this location…"
	case u.fileLoad != nil:
		return "Loading saved scan…"
	}
	return "Scan this location to see what uses space."
}

func thousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (u *ui) fileListTitle() string {
	title := "Largest folders"
	switch u.fileMode.value() {
	case model.Subfolders:
		title = "Largest subfolders"
	case model.Categories:
		title = "Space by category"
	case model.Candidates:
		title = "Review candidates"
	case model.Labeled:
		title = "Largest apps, games and projects"
	case model.LargeFiles:
		title = "Largest files"
	}
	if removal := u.removalFilter(); removal != "" {
		title += " · " + string(removal)
	}
	return title
}

func (u *ui) filesView(gtx layout.Context) layout.Dimensions {
	t := u.th
	if u.fileScope.Changed() {
		u.selectFileScope()
	}
	if u.fileMode.Changed() || u.fileRemoval.Changed() {
		u.renderFiles()
	}
	for i := range u.removalLinks {
		if u.removalLinks[i].Clicked(gtx) {
			u.showRemoval(model.Removals[i])
		}
	}
	if u.browse.Clicked(gtx) {
		u.chooseFolder()
	}
	if u.backButton.Clicked(gtx) {
		u.goBack()
	}
	ready := u.backend != nil
	u.fileScope.disabled = !ready || u.fileCaptureActive()
	context := func(gtx layout.Context) layout.Dimensions {
		return flow{gap: 8, rowGap: 8, middle: true}.Layout(gtx,
			func(gtx layout.Context) layout.Dimensions {
				return labeled(gtx, t, "Location", func(gtx layout.Context) layout.Dimensions { return u.fileScope.Layout(gtx, t) })
			},
			func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &u.browse, buttonStyle{icon: iconFolder, label: "Choose folder…",
					disabled: !ready || u.active != nil || u.choosing})
			},
			func(gtx layout.Context) layout.Dimensions {
				if len(u.back) == 0 {
					return layout.Dimensions{}
				}
				return t.button(gtx, &u.backButton, buttonStyle{kind: quiet, icon: iconChevronLeft, label: "Back",
					disabled: u.fileCaptureActive()})
			},
		)
	}
	children := []layout.FlexChild{layout.Rigid(context), layout.Rigid(space(0, 14))}
	if len(u.fileSummary) > 0 {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.summaryCard(gtx, u.fileSummary) }),
			layout.Rigid(space(0, 16)))
	}
	children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
		return u.results(gtx, &u.filePane, resultsHeader{
			title: u.fileListTitle(),
			count: itemCount(len(u.filePane.rows), u.files.ID != 0),
			notes: u.fileNotes,
			search: func(gtx layout.Context) layout.Dimensions {
				d, changed := u.fileFilter.Layout(gtx, t, u.files.ID == 0)
				if changed {
					u.filePane.search(u.fileFilter.text(), "files")
				}
				return d
			},
			filters: []layout.Widget{
				func(gtx layout.Context) layout.Dimensions { return u.fileMode.Layout(gtx, t) },
				func(gtx layout.Context) layout.Dimensions { return u.fileRemoval.Layout(gtx, t) },
			},
		})
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}
