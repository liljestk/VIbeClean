package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

var (
	fileSizes = []string{"Any size", "1 MiB or more", "64 MiB or more", "1 GiB or more", "10 GiB or more"}
	sizeBytes = []int64{0, units.MiB, 64 * units.MiB, units.GiB, 10 * units.GiB}
	fileAges  = []string{"Any date", "Past day", "Past week", "Past month", "Past year"}
	ageDays   = []int{0, 1, 7, 30, 365}
)

type fileBrowseState struct {
	parent, within, extension string
	size, age, offset, sort   int
	ascending                 bool
}

type fileInventory struct {
	fileParent        string
	fileWithin        searchField
	fileExtension     searchField
	fileSize, fileAge selectBox
	fileFilterToggle  widget.Clickable
	fileFilters       *fileFilterDialog
	filePrevious      widget.Clickable
	fileNext          widget.Clickable
	fileOffset        int
	fileQuery         service.InventoryQuery
	filePage          service.InventoryPage
	inventoryLoad     *guiRead
	inventoryNotice   string
	pendingFileRow    model.RowKey
}

func (u *ui) initInventory() {
	u.fileSize.setOptions(fileSizes, 0)
	u.fileAge.setOptions(fileAges, 0)
}

func (u *ui) usesInventory() bool {
	mode := u.fileMode.value()
	return u.files.Coverage.Inventory && (mode == model.Folders || mode == model.LargeFiles)
}

func (u *ui) fileBrowse() fileBrowseState {
	return fileBrowseState{u.fileParent, u.fileWithin.text(), u.fileExtension.text(),
		u.fileSize.index, u.fileAge.index, u.fileOffset, u.filePane.sortCol, u.filePane.sortAsc}
}

func (u *ui) restoreFileBrowse(s fileBrowseState) {
	u.fileParent, u.fileOffset = s.parent, s.offset
	u.fileWithin.setText(s.within)
	u.fileExtension.setText(s.extension)
	u.fileSize.setOptions(fileSizes, s.size)
	u.fileAge.setOptions(fileAges, s.age)
	u.filePane.sortCol, u.filePane.sortAsc, u.filePane.sorted = s.sort, s.ascending, true
	u.clearInventoryRequest()
}

func (u *ui) clearInventoryRequest() {
	cancelGUIRead(&u.inventoryLoad)
	u.fileQuery = service.InventoryQuery{}
	u.filePage = service.InventoryPage{}
	u.inventoryNotice = ""
	u.pendingFileRow = model.RowKey{}
}

func (u *ui) loadInventory() {
	backend := u.backend
	if backend == nil || !u.usesInventory() {
		return
	}
	files := u.fileMode.value() == model.LargeFiles
	q := service.InventoryQuery{
		CaptureID: u.files.ID, Parent: u.fileParent, Files: files, Query: u.fileFilter.text(),
		Within: strings.TrimSpace(u.fileWithin.text()), MinimumBytes: sizeBytes[max(0, u.fileSize.index)],
		Removal: u.removalFilter(), Offset: u.fileOffset, Limit: service.InventoryPageSize,
		SortColumn: u.filePane.sortCol, Ascending: u.filePane.sortAsc,
	}
	if files {
		q.Extension = strings.TrimSpace(u.fileExtension.text())
	}
	if days := ageDays[max(0, u.fileAge.index)]; days > 0 {
		q.ModifiedSince = u.files.Captured - int64(days)*(24*time.Hour).Milliseconds()
	}
	if q == u.fileQuery {
		return
	}
	if key, ok := u.filePane.selectedKey(); ok && u.fileQuery.Parent == q.Parent && u.fileQuery.CaptureID == q.CaptureID {
		u.pendingFileRow = key
	}
	u.fileQuery = q
	u.inventoryNotice = "Loading matches…"
	u.showFileRows(nil, "", "Loading file details…")
	labels := u.files.Coverage.Labels
	readForGUI(u, &u.inventoryLoad, "gui.file_inventory", func(ctx context.Context) (service.InventoryPage, error) {
		return backend.Inventory(ctx, q)
	}, func(page service.InventoryPage, err error) {
		u.filePage = page
		if err != nil {
			u.inventoryNotice = err.Error()
			u.showFileRows(nil, "", "File details are unavailable.")
			return
		}
		u.inventoryNotice = ""
		rows := make([]displayRow, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, fileRow(item, model.CaptureRemoval(item.Kind, labels), false))
		}
		u.showFileRows(rows, "", "No matches in this location.")
		if u.pendingFileRow != (model.RowKey{}) {
			u.filePane.restoreSelection(u.pendingFileRow)
			u.pendingFileRow = model.RowKey{}
		}
	})
}

func (u *ui) inventorySort() {
	u.fileOffset = 0
	u.loadInventory()
}

func (u *ui) fileFilterCount() int {
	count := 0
	if u.removalFilter() != "" {
		count++
	}
	if !u.usesInventory() {
		return count
	}
	for _, on := range []bool{u.fileWithin.text() != "", u.fileMode.value() == model.LargeFiles && u.fileExtension.text() != "",
		u.fileSize.index > 0, u.fileAge.index > 0} {
		if on {
			count++
		}
	}
	return count
}

func (u *ui) fileFilterButton(gtx layout.Context) layout.Dimensions {
	label := "Filters…"
	if count := u.fileFilterCount(); count != 0 {
		label = fmt.Sprintf("Filters (%d)", count)
	}
	return u.th.button(gtx, &u.fileFilterToggle, buttonStyle{label: label, disabled: u.files.ID == 0})
}

type fileFilterDialog struct {
	inventory, files, focused bool
	within, extension         searchField
	removal, size, age        selectBox
	clear, cancel, apply      widget.Clickable
	problem                   string
	scrim                     bool
}

func (u *ui) openFileFilters() {
	d := &fileFilterDialog{inventory: u.usesInventory(), files: u.fileMode.value() == model.LargeFiles}
	d.within = searchField{plain: true, prompt: "Folder path"}
	d.extension = searchField{plain: true, prompt: "Extension, e.g. .zip"}
	d.within.setText(u.fileWithin.text())
	d.extension.setText(u.fileExtension.text())
	d.removal.name, d.size.name, d.age.name = "Cleanup guidance", "Minimum size", "Modified date"
	d.removal.setOptions(u.fileRemoval.options, u.fileRemoval.index)
	d.size.setOptions(fileSizes, u.fileSize.index)
	d.age.setOptions(fileAges, u.fileAge.index)
	u.fileFilters = d
}

func (u *ui) closeFileFilters(gtx layout.Context) {
	u.fileFilters = nil
	u.returnFocus = &u.fileFilterToggle
	gtx.Execute(op.InvalidateCmd{})
}

func (d *fileFilterDialog) closePicker() bool {
	for _, box := range []*selectBox{&d.removal, &d.size, &d.age} {
		if box.open {
			box.open = false
			return true
		}
	}
	return false
}

func (d *fileFilterDialog) applyTo(gtx layout.Context, u *ui) {
	within := strings.Trim(strings.TrimSpace(d.within.text()), "\"")
	if d.inventory && within != "" && !filepath.IsAbs(within) {
		d.problem = `Enter a full folder path, such as C:\Users\Public.`
		gtx.Execute(key.FocusCmd{Tag: &d.within.editor})
		return
	}
	before, removal := u.fileBrowse(), u.fileRemoval.index
	u.fileRemoval.setOptions(u.fileRemoval.options, d.removal.index)
	if d.inventory {
		u.fileWithin.setText(within)
		u.fileSize.setOptions(fileSizes, d.size.index)
		u.fileAge.setOptions(fileAges, d.age.index)
		if d.files {
			u.fileExtension.setText(strings.TrimSpace(d.extension.text()))
		}
	}
	u.closeFileFilters(gtx)
	if before != u.fileBrowse() || removal != u.fileRemoval.index {
		u.fileOffset = 0
		u.renderFiles()
	}
}

func (d *fileFilterDialog) layout(gtx layout.Context, u *ui) {
	if d.cancel.Clicked(gtx) {
		u.closeFileFilters(gtx)
		return
	}
	if d.apply.Clicked(gtx) {
		d.applyTo(gtx, u)
		if u.fileFilters == nil {
			return
		}
	}
	if d.clear.Clicked(gtx) {
		d.within.setText("")
		d.extension.setText("")
		d.removal.setOptions(d.removal.options, 0)
		d.size.setOptions(fileSizes, 0)
		d.age.setOptions(fileAges, 0)
		d.problem = ""
	}
	if !d.focused {
		gtx.Execute(key.FocusCmd{Tag: &d.removal.button})
		d.focused = true
	}
	t := u.th
	field := func(label string, control layout.Widget) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.small(t.quiet), label) }),
				layout.Rigid(space(0, 4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return control(gtx)
				}),
			)
		}
	}
	pick := func(box *selectBox) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			dims := box.Layout(gtx, t)
			box.Changed()
			return dims
		}
	}
	entry := func(input *searchField) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			dims, _ := input.Layout(gtx, t, false)
			return dims
		}
	}
	modal(gtx, t, &d.scrim, 520, 0, func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(17, t.ink), "Filters")
			}),
			layout.Rigid(space(0, 14)),
			layout.Rigid(field("Cleanup guidance", pick(&d.removal))),
		}
		if d.inventory {
			children = append(children,
				layout.Rigid(space(0, 10)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{}.Layout(gtx,
						layout.Flexed(1, field("Minimum size", pick(&d.size))),
						layout.Rigid(space(12, 0)),
						layout.Flexed(1, field("Modified date", pick(&d.age))),
					)
				}),
				layout.Rigid(space(0, 10)),
				layout.Rigid(field("Within folder", entry(&d.within))),
			)
			if d.files {
				children = append(children, layout.Rigid(space(0, 10)), layout.Rigid(field("Extension", entry(&d.extension))))
			}
		}
		if d.problem != "" {
			children = append(children, layout.Rigid(space(0, 6)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.grow).wrapped(2), d.problem)
			}))
		}
		children = append(children, layout.Rigid(space(0, 16)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return spread(gtx, 8,
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &d.clear, buttonStyle{kind: quiet, label: "Clear filters"})
				},
				func(gtx layout.Context) layout.Dimensions {
					return flow{gap: 8, rowGap: 8}.Layout(gtx,
						func(gtx layout.Context) layout.Dimensions {
							return t.button(gtx, &d.cancel, buttonStyle{kind: quiet, label: "Cancel"})
						},
						func(gtx layout.Context) layout.Dimensions {
							return t.button(gtx, &d.apply, buttonStyle{kind: primary, label: "Apply filters"})
						},
					)
				})
		}))
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (u *ui) inventoryPager(gtx layout.Context) layout.Dimensions {
	t := u.th
	last := min(u.fileOffset+len(u.filePage.Items), u.filePage.Total)
	page := fmt.Sprintf("%s–%s of %s", thousands(int64(u.fileOffset+1)), thousands(int64(last)), thousands(int64(u.filePage.Total)))
	if len(u.filePane.missingPaths) > 0 {
		page += " in scan"
	}
	return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return flow{gap: 8, rowGap: 6, middle: true}.Layout(gtx,
			func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &u.filePrevious, buttonStyle{icon: iconChevronLeft, label: "Previous",
					compact: true, disabled: u.fileOffset == 0 || u.inventoryLoad != nil})
			},
			func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.quiet), page)
			},
			func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &u.fileNext, buttonStyle{icon: iconChevronRight, label: "Next",
					compact: true, disabled: last >= u.filePage.Total || u.inventoryLoad != nil})
			},
		)
	})
}

func (u *ui) fileResultsTitle() string {
	if u.fileParent == "" || !u.usesInventory() {
		if u.usesInventory() && u.fileMode.value() == model.Folders {
			return "Folders and files"
		}
		return u.fileListTitle()
	}
	name := filepath.Base(u.fileParent)
	if u.fileMode.value() == model.LargeFiles {
		return "Largest files in " + name
	}
	return "Contents of " + name
}
