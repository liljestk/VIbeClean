package ui

import (
	"io"
	"strings"

	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/report"
)

type exportDialog struct {
	report report.Report
	csv    widget.Clickable
	json   widget.Clickable
	close  widget.Clickable
	scrim  bool
}

func (u *ui) reportSource() model.Snapshot {
	switch u.view {
	case viewFiles:
		if u.inventoryLoad == nil && u.inventoryNotice == "" {
			return u.files
		}
	case viewChanges:
		if u.comparison.Baseline.ID != 0 && u.compareLoad == nil && u.historyLoad == nil {
			return u.comparison.Latest
		}
	case viewDocker:
		return u.docker
	}
	return model.Snapshot{}
}

func (u *ui) openExport() {
	pane, saved := u.activePane(), u.reportSource()
	if u.exporting || pane == nil || saved.ID == 0 {
		return
	}
	saved.Records = nil
	r := report.Report{View: viewNames[u.view], Latest: saved, Count: len(pane.rows)}
	filters := []string{u.activeSearch().text()}
	switch u.view {
	case viewFiles:
		r.Series = u.fileMode.value()
		filters = append(filters, u.fileRemoval.value())
		if u.usesInventory() {
			filters = append(filters, u.fileQuery.Parent, u.fileQuery.Within, u.fileQuery.Extension, u.fileSize.value(), u.fileAge.value())
			ctx, backend, query := u.ctx, u.backend, u.fileQuery
			missing := pane.missingPaths
			r.Count = u.filePage.Total - len(missing)
			r.Stream = func(emit func(report.Row) error) error {
				return backend.StreamInventory(ctx, query, func(item model.Record) error {
					if missing[item.Path] {
						return nil
					}
					mark := model.CaptureRemoval(item.Kind, saved.Coverage.Labels)
					return emit(report.Row{Item: item, Removal: removalTitle(mark)})
				})
			}
		}
	case viewChanges:
		r.Series = u.changeMode.value()
		baseline := u.comparison.Baseline
		baseline.Records = nil
		r.Baseline, r.ShortWindow, r.Omitted = &baseline, u.comparison.ShortWindow, u.comparison.Omitted
		filters = append(filters, u.direction.value(), "≥ "+u.minimum.value())
		if u.focused != nil {
			filters = append(filters, u.focused.path)
		}
	case viewDocker:
		r.Series = u.dockerMode.value()
		if u.idleApplies() && u.idleOnly.value.Value {
			filters = append(filters, "Idle only")
		}
		if u.dockerProject != "" {
			filters = append(filters, u.dockerProject)
		}
	}
	r.Filters = strings.Trim(strings.Join(filters, " · "), " ·")
	if r.Stream == nil {
		r.Rows = make([]report.Row, 0, r.Count)
		for _, index := range pane.rows {
			display := pane.items[index]
			row := report.Row{Item: display.Item, Removal: removalTitle(display.Mark)}
			if pane.changes {
				change := &report.Change{Bytes: display.Delta, Bounded: display.Before < 0 || display.After < 0}
				if display.Before >= 0 {
					change.Before = &display.Before
				}
				if display.After >= 0 {
					change.After = &display.After
				}
				row.Change = change
			}
			r.Rows = append(r.Rows, row)
		}
	}
	u.export = &exportDialog{report: r}
}

func (d *exportDialog) layout(gtx layout.Context, u *ui) {
	t := u.th
	switch {
	case d.csv.Clicked(gtx):
		u.saveReport(d.report, "csv", "CSV report", d.report.WriteCSV)
		return
	case d.json.Clicked(gtx):
		u.saveReport(d.report, "json", "JSON report", d.report.WriteJSON)
		return
	case d.close.Clicked(gtx):
		u.export = nil
		return
	}
	modal(gtx, t, &d.scrim, 460, 0, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(17, t.ink), "Export current view")
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.quiet).wrapped(-1),
					d.report.Latest.Label+" · "+itemCount(d.report.Count, true)+
						" match the current view. The file includes paths, scan times, and coverage.")
			}),
			layout.Rigid(space(0, 16)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flow{gap: 8, rowGap: 8, end: true}.Layout(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.close, buttonStyle{kind: quiet, label: "Cancel"})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.json, buttonStyle{label: "Save JSON…"})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.csv, buttonStyle{kind: primary, label: "Save CSV…"})
					},
				)
			}),
		)
	})
}

func (u *ui) saveReport(r report.Report, extension, format string, write func(io.Writer) error) {
	u.export, u.exporting = nil, true
	hwnd := u.hwnd
	go func() {
		name := "Space Review - " + r.View + "." + extension
		path, err := saveFileDialog(hwnd, "Export "+r.View, name, format, "*."+extension, extension)
		if err == nil && path != "" {
			err = writeNewFile(path, write)
		}
		u.post(func() {
			u.exporting = false
			switch {
			case err != nil:
				u.showAlert("Could not save report", err.Error())
			case path != "":
				u.showAlert("Report saved", path)
			}
		})
	}()
}
