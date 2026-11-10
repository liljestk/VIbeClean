package ui

import (
	"image"

	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
)

// resultsHeader configures the list heading, filters and breadcrumb above a
// ranked table.
type resultsHeader struct {
	title, count, notes string
	back                *widget.Clickable
	backText            string
	backDisabled        bool
	search              layout.Widget
	filters             []layout.Widget
	footer              layout.Widget
}

func (u *ui) canShowMap() bool {
	return u.view == viewFiles && u.usesInventory()
}

func (u *ui) toggleDetails(showMap bool) {
	if !u.wideResults {
		u.details = &detailsDialog{showMap: showMap}
		return
	}
	if showMap {
		u.fileMap.open = !u.fileMap.open
	} else if u.canShowMap() && u.fileMap.open {
		u.fileMap.open = false
		u.inspectorHidden = false
	} else {
		u.inspectorHidden = !u.inspectorHidden
	}
}

// results keeps the toolbar above both the table and the optional side pane.
func (u *ui) results(gtx layout.Context, p *rankedPane, h resultsHeader) layout.Dimensions {
	t := u.th
	if u.inspectorButton.Clicked(gtx) {
		u.toggleDetails(false)
	}
	if u.canShowMap() && u.fileMap.toggle.Clicked(gtx) {
		u.toggleDetails(true)
	}
	mapOpen := u.wideResults && u.canShowMap() && u.fileMap.open
	showDetails := u.wideResults && !u.inspectorHidden && !mapOpen
	heading := func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{}
		if h.back != nil {
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, h.back, buttonStyle{kind: quiet, icon: iconChevronLeft, label: "Back", compact: true,
					accessibleName: h.backText, disabled: h.backDisabled})
			}), layout.Rigid(space(6, 0)))
		}
		children = append(children,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(15, t.ink), h.title)
			}),
			layout.Rigid(space(8, 0)),
		)
		if h.count != "" {
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.quiet), h.count)
			}), layout.Rigid(space(8, 0)))
		}
		if u.canShowMap() {
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &u.fileMap.toggle, buttonStyle{kind: quiet, icon: iconBoxes, label: "Treemap",
					compact: true, pressed: mapOpen})
			}))
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.button(gtx, &u.inspectorButton, buttonStyle{kind: quiet, icon: iconPanel, label: "Details",
				shortcut: "Ctrl+I", compact: true, pressed: showDetails})
		}))
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx, children...)
	}
	table := func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return p.layoutTable(gtx, t)
		})}
		if h.footer != nil {
			children = append(children, layout.Rigid(h.footer))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	}
	body := func(gtx layout.Context) layout.Dimensions {
		if !mapOpen && !showDetails {
			return table(gtx)
		}
		iw := max(gtx.Dp(250), min(gtx.Dp(340), gtx.Constraints.Max.X*3/10))
		return layout.Flex{}.Layout(gtx,
			layout.Flexed(1, table),
			layout.Rigid(space(16, 0)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints = layout.Exact(image.Pt(iw, gtx.Constraints.Max.Y))
				return u.layoutResultDetails(gtx, p, mapOpen)
			}),
		)
	}
	children := []layout.FlexChild{layout.Rigid(heading)}
	if h.notes != "" {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.small(t.quiet).wrapped(2), h.notes)
		}))
	}
	children = append(children,
		layout.Rigid(space(0, 6)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return toolbar(gtx, 144, h.search, h.filters...) }),
		layout.Rigid(space(0, 8)),
		layout.Flexed(1, body),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (u *ui) layoutResultDetails(gtx layout.Context, p *rankedPane, showMap bool) layout.Dimensions {
	if showMap {
		return u.th.panel(gtx, layout.UniformInset(12), 10, u.th.chrome, u.layoutFileMap)
	}
	return p.layoutInspector(gtx, u)
}

type detailsDialog struct {
	showMap bool
	summary []metric
	focused bool
	close   widget.Clickable
	scrim   bool
}

func (u *ui) closeDetails(gtx layout.Context) {
	target := &u.inspectorButton
	switch {
	case u.details.summary != nil:
		target = &u.summaryButton
	case u.details.showMap:
		target = &u.fileMap.toggle
	}
	u.details = nil
	u.returnFocus = target
	gtx.Execute(op.InvalidateCmd{})
}

func (d *detailsDialog) layout(gtx layout.Context, u *ui) {
	if d.close.Clicked(gtx) || (d.showMap && !u.canShowMap()) {
		u.closeDetails(gtx)
		return
	}
	if d.summary != nil && u.view == viewFiles && u.fileSummaryActions(gtx) {
		u.closeDetails(gtx)
		return
	}
	if !d.focused {
		gtx.Execute(key.FocusCmd{Tag: &d.close})
		d.focused = true
	}
	t := u.th
	title := "Details"
	height := float32(530)
	body := layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
		return u.layoutResultDetails(gtx, u.activePane(), d.showMap)
	})
	if d.showMap {
		title = "Treemap"
	}
	if d.summary != nil {
		title, height = "Scan summary", 0
		body = layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.summaryGrid(gtx, d.summary) })
	}
	modal(gtx, t, &d.scrim, 560, height, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return spread(gtx, 8,
					func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(17, t.ink), title) },
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.close, buttonStyle{label: "Close", compact: true})
					})
			}),
			layout.Rigid(space(0, 8)),
			body,
		)
	})
}

type recorded struct {
	call op.CallOp
	size image.Point
}

func record(gtx layout.Context, w layout.Widget) recorded {
	rec := op.Record(gtx.Ops)
	dims := w(gtx)
	return recorded{rec.Stop(), dims.Size}
}

// place draws recorded widgets left to right from y, wrapping at maxW, and
// returns the height used.
func place(gtx layout.Context, items []recorded, x0, y, maxW, gap, rowGap int) int {
	top := y
	var line []recorded
	// flush draws one line with its items centered vertically.
	flush := func() int {
		h := 0
		for _, item := range line {
			h = max(h, item.size.Y)
		}
		x := x0
		for _, item := range line {
			stack := op.Offset(image.Pt(x, y+(h-item.size.Y)/2)).Push(gtx.Ops)
			item.call.Add(gtx.Ops)
			stack.Pop()
			x += item.size.X + gap
		}
		return h
	}
	width := x0
	for _, item := range items {
		if len(line) > 0 && width+item.size.X > maxW {
			y += flush() + rowGap
			line, width = nil, x0
		}
		line = append(line, item)
		width += item.size.X + gap
	}
	if len(line) > 0 {
		y += flush()
	}
	return y - top
}

// toolbar gives the search field the room left by the other controls, down
// to minSearch, and moves the controls to a second line below that.
func toolbar(gtx layout.Context, minSearch unit.Dp, search layout.Widget, controls ...layout.Widget) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	gap := gtx.Dp(8)
	cgtx := gtx
	cgtx.Constraints.Min = image.Point{}
	items := make([]recorded, 0, len(controls))
	total := 0
	for _, control := range controls {
		r := record(cgtx, control)
		if r.size.X == 0 {
			continue
		}
		items = append(items, r)
		total += r.size.X + gap
	}
	if search == nil {
		h := place(gtx, items, 0, 0, maxW, gap, gap)
		return layout.Dimensions{Size: image.Pt(maxW, h)}
	}
	rest := maxW - total
	if rest >= gtx.Dp(minSearch) {
		sgtx := gtx
		sgtx.Constraints = layout.Constraints{Min: image.Pt(rest, 0), Max: image.Pt(rest, gtx.Constraints.Max.Y)}
		s := record(sgtx, search)
		h := s.size.Y
		for _, item := range items {
			h = max(h, item.size.Y)
		}
		stack := op.Offset(image.Pt(0, (h-s.size.Y)/2)).Push(gtx.Ops)
		s.call.Add(gtx.Ops)
		stack.Pop()
		x := rest + gap
		for _, item := range items {
			stack := op.Offset(image.Pt(x, (h-item.size.Y)/2)).Push(gtx.Ops)
			item.call.Add(gtx.Ops)
			stack.Pop()
			x += item.size.X + gap
		}
		return layout.Dimensions{Size: image.Pt(maxW, h)}
	}
	sgtx := gtx
	sgtx.Constraints = layout.Constraints{Min: image.Pt(maxW, 0), Max: image.Pt(maxW, gtx.Constraints.Max.Y)}
	s := record(sgtx, search)
	s.call.Add(gtx.Ops)
	y := s.size.Y + gap
	h := place(gtx, items, 0, y, maxW, gap, gap)
	return layout.Dimensions{Size: image.Pt(maxW, y+h)}
}

// summaryCard shows readings in an even grid that wraps on narrow windows.
// Short windows get a denser card so the results keep most of the height.
// Readings with a click target highlight on hover and show a chevron.
func (u *ui) summaryCard(gtx layout.Context, metrics []metric) layout.Dimensions {
	if len(metrics) > 3 && gtx.Metric.PxToDp(gtx.Constraints.Max.X) < 680 &&
		gtx.Metric.PxToDp(gtx.Constraints.Max.Y) < 400 {
		return u.compactSummary(gtx, metrics)
	}
	return u.summaryGrid(gtx, metrics)
}

func (u *ui) compactSummary(gtx layout.Context, metrics []metric) layout.Dimensions {
	t := u.th
	if u.summaryButton.Clicked(gtx) {
		u.details = &detailsDialog{summary: metrics}
	}
	return t.panel(gtx, layout.Inset{Left: 12, Right: 6, Top: 4, Bottom: 4}, 8, t.chrome,
		func(gtx layout.Context) layout.Dimensions {
			return spread(gtx, 8,
				func(gtx layout.Context) layout.Dimensions {
					return t.label(gtx, t.strong(13, t.ink), metrics[0].label+" · "+metrics[0].value)
				},
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.summaryButton, buttonStyle{kind: quiet, label: "Summary…", compact: true})
				})
		})
}

func (u *ui) summaryGrid(gtx layout.Context, metrics []metric) layout.Dimensions {
	t := u.th
	n := len(metrics)
	if n == 0 {
		return layout.Dimensions{}
	}
	dense := gtx.Metric.PxToDp(gtx.Constraints.Max.Y) < 560
	w := gtx.Constraints.Max.X
	minCell := unit.Dp(165)
	if dense {
		minCell = 120
	}
	// Balance the rows: four readings wrap as two by two, not three and one.
	fit := max(1, min(n, w/gtx.Dp(minCell)))
	rows := (n + fit - 1) / fit
	cols := (n + rows - 1) / rows
	cellW := w / cols
	pad := layout.Inset{Top: 11, Bottom: 11, Left: 14, Right: 14}
	valueSize := unit.Sp(20)
	if dense {
		pad = layout.Inset{Top: 7, Bottom: 7, Left: 12, Right: 12}
		valueSize = 16
	}
	cells := make([]recorded, n)
	for i, m := range metrics {
		cgtx := gtx
		cgtx.Constraints = layout.Constraints{Min: image.Pt(cellW, 0), Max: image.Pt(cellW, gtx.Constraints.Max.Y)}
		heading := func(gtx layout.Context) layout.Dimensions {
			var parts []layout.FlexChild
			if m.mark != "" {
				parts = append(parts, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					dot := gtx.Dp(8)
					paint.FillShape(gtx.Ops, t.removal(m.mark), clip.Ellipse{Max: image.Pt(dot, dot)}.Op(gtx.Ops))
					return layout.Dimensions{Size: image.Pt(dot, dot)}
				}), layout.Rigid(space(6, 0)))
			}
			parts = append(parts, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(12, t.quiet), m.label) }))
			if m.click != nil {
				parts = append(parts, layout.Rigid(space(2, 0)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return drawIcon(gtx, iconChevronRight, 13, t.quiet)
				}))
			}
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx, parts...)
		}
		content := func(gtx layout.Context) layout.Dimensions {
			return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				children := []layout.FlexChild{
					layout.Rigid(heading),
					layout.Rigid(space(0, 2)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(valueSize, t.ink), m.value) }),
				}
				if m.caption != "" && !dense {
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return t.label(gtx, t.small(t.quiet), m.caption)
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			})
		}
		cells[i] = record(cgtx, func(gtx layout.Context) layout.Dimensions {
			if m.click == nil {
				return content(gtx)
			}
			return m.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				body := record(gtx, content)
				defer t.exposeButton(gtx, body.size, m.click, m.label+", "+m.value).Pop()
				if m.click.Hovered() || m.click.Pressed() {
					fill(gtx, t.well, body.size, 8)
				}
				body.call.Add(gtx.Ops)
				pointer.CursorPointer.Add(gtx.Ops)
				if gtx.Focused(m.click) {
					t.focusRing(gtx, body.size, 8)
				}
				return layout.Dimensions{Size: body.size}
			})
		})
	}
	heights := make([]int, rows)
	for i, c := range cells {
		heights[i/cols] = max(heights[i/cols], c.size.Y)
	}
	total := 0
	for _, h := range heights {
		total += h
	}
	fill(gtx, t.chrome, image.Pt(w, total), 10)
	y := 0
	for r := 0; r < rows; r++ {
		for c := 0; c < cols && r*cols+c < n; c++ {
			stack := op.Offset(image.Pt(c*cellW, y)).Push(gtx.Ops)
			cells[r*cols+c].call.Add(gtx.Ops)
			stack.Pop()
		}
		y += heights[r]
	}
	return layout.Dimensions{Size: image.Pt(w, total)}
}

func clipRect(size image.Point) clip.Op { return clip.Rect{Max: size}.Op() }

func clipRRect(size image.Point, radius int, gtx layout.Context) clip.Op {
	return clip.UniformRRect(image.Rectangle{Max: size}, radius).Op(gtx.Ops)
}
