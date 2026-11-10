package ui

import (
	"strings"

	"gioui.org/layout"
	"gioui.org/text"
	"gioui.org/widget"
)

type coverageLine struct {
	text    string
	heading bool
}

type coverageDialog struct {
	lines       []coverageLine
	list        widget.List
	close, copy widget.Clickable
	copied      bool
	scrim       bool
}

func (u *ui) showScanCoverage() {
	saved := u.files
	d := &coverageDialog{lines: []coverageLine{{text: scanCoverageText(saved)}}}
	add := func(heading string, paths []string) {
		if len(paths) == 0 {
			return
		}
		d.lines = append(d.lines, coverageLine{heading, true})
		for _, path := range paths {
			d.lines = append(d.lines, coverageLine{text: path})
		}
	}
	add("Unreadable locations", saved.Coverage.Gaps)
	label := "Estimated directories"
	if int64(len(saved.Coverage.EstimatePaths)) < saved.Coverage.Estimated {
		label = "Sample of estimated directories"
	}
	add(label, saved.Coverage.EstimatePaths)
	add("Excluded from this scan", saved.Coverage.Exclusions)
	u.coverageInfo = d
}

func (d *coverageDialog) layout(gtx layout.Context, u *ui) {
	if d.close.Clicked(gtx) {
		u.coverageInfo = nil
		return
	}
	if d.copy.Clicked(gtx) {
		var text strings.Builder
		for _, line := range d.lines {
			text.WriteString(line.text + "\n")
		}
		u.copyText(text.String())
		d.copied = true
	}
	t := u.th
	modal(gtx, t, &d.scrim, 620, 420, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(18, t.ink), "Scan coverage")
			}),
			layout.Rigid(space(0, 8)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				d.list.Axis = layout.Vertical
				return d.list.List.Layout(gtx, len(d.lines), func(gtx layout.Context, i int) layout.Dimensions {
					line := d.lines[i]
					style := t.body(t.ink).wrapped(-1)
					style.wrap = text.WrapGraphemes
					if line.heading {
						return layout.Inset{Top: 12, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return t.label(gtx, t.strong(14, t.ink), line.text)
						})
					}
					return t.label(gtx, style, line.text)
				})
			}),
			layout.Rigid(space(0, 12)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flow{gap: 8, rowGap: 6, end: true}.Layout(gtx,
					func(gtx layout.Context) layout.Dimensions {
						label := "Copy details"
						if d.copied {
							label = "Copied"
						}
						return t.button(gtx, &d.copy, buttonStyle{icon: iconCopy, label: label})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.close, buttonStyle{kind: primary, label: "Close"})
					},
				)
			}),
		)
	})
}
