package ui

import (
	"image"

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
	search              layout.Widget
	filters             []layout.Widget
}

// results lays out a ranked table with its inspector. The inspector sits
// beside the table on wide windows and below it on narrow ones.
func (u *ui) results(gtx layout.Context, p *rankedPane, h resultsHeader) layout.Dimensions {
	t := u.th
	widthDp := gtx.Metric.PxToDp(gtx.Constraints.Max.X)
	column := func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return spread(gtx, 12,
					func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(15, t.ink), h.title) },
					func(gtx layout.Context) layout.Dimensions {
						if h.count == "" {
							return layout.Dimensions{}
						}
						return t.label(gtx, t.small(t.quiet), h.count)
					})
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return toolbar(gtx, 200, h.search, h.filters...)
			}),
		}
		if h.back != nil {
			children = append(children, layout.Rigid(space(0, 8)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return t.button(gtx, h.back, buttonStyle{kind: quiet, icon: iconChevronLeft, label: h.backText, compact: true})
			}))
		}
		if h.notes != "" {
			children = append(children, layout.Rigid(space(0, 8)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.quiet).wrapped(2), h.notes)
			}))
		}
		children = append(children, layout.Rigid(space(0, 10)), layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return p.layoutTable(gtx, t)
		}))
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	}
	if u.inspectorHidden {
		return column(gtx)
	}
	// Side by side while the table keeps room for its name and amount columns.
	if widthDp >= 680 {
		iw := max(gtx.Dp(250), min(gtx.Dp(340), gtx.Constraints.Max.X*3/10))
		return layout.Flex{}.Layout(gtx,
			layout.Flexed(1, column),
			layout.Rigid(space(16, 0)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints = layout.Exact(image.Pt(iw, gtx.Constraints.Max.Y))
				return p.layoutInspector(gtx, u, false)
			}),
		)
	}
	ih := max(gtx.Dp(190), min(gtx.Dp(260), gtx.Constraints.Max.Y*2/5))
	if gtx.Constraints.Max.Y-ih < gtx.Dp(300) {
		// Too short to stack: the table keeps the space.
		return column(gtx)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, column),
		layout.Rigid(space(0, 12)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(gtx.Constraints.Max.X, ih))
			return p.layoutInspector(gtx, u, widthDp >= 520)
		}),
	)
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
