package ui

import (
	"image"
	"strings"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/model"
)

type exclusionsDialog struct {
	scope   string
	entry   searchField
	problem string
	apply   widget.Clickable
	cancel  widget.Clickable
	scrim   bool
	focused bool
}

func (u *ui) openExclusions() {
	if u.backend == nil || u.active != nil {
		return
	}
	scope := u.fileScopeValue()
	d := &exclusionsDialog{scope: scope}
	d.entry.editor.SetText(strings.Join(u.fileCaptureSettings(scope).Exclusions, "\n"))
	u.exclusions = d
}

func (d *exclusionsDialog) layout(gtx layout.Context, u *ui) {
	if d.cancel.Clicked(gtx) {
		u.exclusions = nil
		return
	}
	if d.apply.Clicked(gtx) {
		paths, err := model.ScanExclusions(d.scope, strings.Split(d.entry.text(), "\n"))
		if err != nil {
			d.problem = err.Error()
		} else {
			u.exclusions = nil
			u.startFileScan(d.scope, u.fileCaptureSettings(d.scope).Kind, false, paths, true, "")
			return
		}
	}
	t := u.th
	modal(gtx, t, &d.scrim, 560, 0, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(17, t.ink), "Scan exclusions")
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.quiet).wrapped(4),
					d.scope+"\nEnter one absolute file or folder path per line. The new scan skips these paths. Comparisons use matching exclusions.")
			}),
			layout.Rigid(space(0, 10)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(104))
				fill(gtx, t.surface, size, 7)
				color := t.line
				if gtx.Focused(&d.entry.editor) {
					color = t.accent
				}
				outline(gtx, color, size, 7, 1)
				if !d.focused {
					gtx.Execute(key.FocusCmd{Tag: &d.entry.editor})
					d.focused = true
				}
				t.expose(gtx, size, accessKey{tag: &d.entry.editor},
					accessNode{name: "Paths to exclude", value: d.entry.text(), role: accessEdit, tag: &d.entry.editor,
						setValue: d.entry.setText})
				gtx.Constraints = layout.Exact(size)
				return layout.UniformInset(8).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return d.entry.editorLayout(gtx, t, t.body(t.ink))
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.grow).wrapped(2), d.problem)
			}),
			layout.Rigid(space(0, 10)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flow{gap: 8, rowGap: 8, end: true}.Layout(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.cancel, buttonStyle{kind: quiet, label: "Cancel"})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.apply, buttonStyle{kind: primary, label: "Apply and scan"})
					},
				)
			}),
		)
	})
}
