package ui

import (
	"image"
	"image/color"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
)

// fill paints a rounded rectangle of the given size.
func fill(gtx layout.Context, c color.NRGBA, size image.Point, radius unit.Dp) {
	if c.A == 0 {
		return
	}
	paint.FillShape(gtx.Ops, c, clip.UniformRRect(image.Rectangle{Max: size}, gtx.Dp(radius)).Op(gtx.Ops))
}

// outline strokes the inside edge of a rounded rectangle.
func outline(gtx layout.Context, c color.NRGBA, size image.Point, radius, width unit.Dp) {
	w := float32(gtx.Dp(width))
	if w <= 0 {
		w = 1
	}
	inset := w / 2
	rect := image.Rectangle{Max: size}
	rr := clip.RRect{Rect: rect, NW: gtx.Dp(radius), NE: gtx.Dp(radius), SW: gtx.Dp(radius), SE: gtx.Dp(radius)}
	_ = inset
	paint.FillShape(gtx.Ops, c, clip.Stroke{Path: insetRRect(gtx.Ops, rr, inset), Width: w}.Op())
}

func insetRRect(ops *op.Ops, rr clip.RRect, inset float32) clip.PathSpec {
	r := float32(rr.NW)
	minX, minY := float32(rr.Rect.Min.X)+inset, float32(rr.Rect.Min.Y)+inset
	maxX, maxY := float32(rr.Rect.Max.X)-inset, float32(rr.Rect.Max.Y)-inset
	r = max(0, min(r-inset, (maxX-minX)/2, (maxY-minY)/2))
	var p clip.Path
	p.Begin(ops)
	p.MoveTo(f32.Pt(minX+r, minY))
	p.LineTo(f32.Pt(maxX-r, minY))
	p.QuadTo(f32.Pt(maxX, minY), f32.Pt(maxX, minY+r))
	p.LineTo(f32.Pt(maxX, maxY-r))
	p.QuadTo(f32.Pt(maxX, maxY), f32.Pt(maxX-r, maxY))
	p.LineTo(f32.Pt(minX+r, maxY))
	p.QuadTo(f32.Pt(minX, maxY), f32.Pt(minX, maxY-r))
	p.LineTo(f32.Pt(minX, minY+r))
	p.QuadTo(f32.Pt(minX, minY), f32.Pt(minX+r, minY))
	p.Close()
	return p.End()
}

// focusRing marks keyboard focus around a control.
func (t *theme) focusRing(gtx layout.Context, size image.Point, radius unit.Dp) {
	outline(gtx, t.accent, size, radius, 2)
}

// space is a fixed gap.
func space(w, h unit.Dp) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Dimensions{Size: image.Pt(gtx.Dp(w), gtx.Dp(h))}
	}
}

// panel lays out content on a rounded chrome surface.
func (t *theme) panel(gtx layout.Context, pad layout.Inset, radius unit.Dp, c color.NRGBA, w layout.Widget) layout.Dimensions {
	record := op.Record(gtx.Ops)
	dims := pad.Layout(gtx, w)
	call := record.Stop()
	fill(gtx, c, dims.Size, radius)
	call.Add(gtx.Ops)
	return dims
}

// buttonKind selects a button's emphasis.
type buttonKind int

const (
	secondary buttonKind = iota
	primary
	quiet
)

type buttonStyle struct {
	kind     buttonKind
	icon     icon
	label    string
	disabled bool
	// pressed shows a toggle button's on state.
	pressed bool
	// compact uses a smaller control for secondary actions.
	compact bool
}

// button lays out a clickable button sized to its label, so labels are never
// truncated while there is room.
func (t *theme) button(gtx layout.Context, c *widget.Clickable, s buttonStyle) layout.Dimensions {
	if s.disabled {
		gtx = gtx.Disabled()
	}
	height, padX, size := unit.Dp(34), unit.Dp(13), unit.Sp(14)
	if s.compact {
		height, padX, size = 30, 10, 13
	}
	return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		fg := t.ink
		bg, edge := t.surface, t.line
		switch s.kind {
		case primary:
			bg, edge, fg = t.accent, t.accent, t.onAccent
			if c.Hovered() || c.Pressed() {
				bg = mix(t.accent, t.ink, 0.14)
				edge = bg
			}
		case quiet:
			bg, edge = color.NRGBA{}, color.NRGBA{}
			if c.Hovered() || c.Pressed() || s.pressed {
				bg = t.well
			}
		default:
			if c.Hovered() || c.Pressed() || s.pressed {
				bg = t.well
			}
		}
		if t.contrast && s.pressed {
			bg, fg = t.selected, t.onAccent
		}
		record := op.Record(gtx.Ops)
		inner := layout.Inset{Left: padX, Right: padX}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = image.Point{}
			children := []layout.FlexChild{}
			if s.icon != nil {
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return drawIcon(gtx, s.icon, 16, fg)
				}))
				if s.label != "" {
					children = append(children, layout.Rigid(space(7, 0)))
				}
			}
			if s.label != "" {
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return t.label(gtx, style{size: size, weight: font.SemiBold, color: fg}, s.label)
				}))
			}
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx, children...)
		})
		call := record.Stop()
		h := max(gtx.Dp(height), inner.Size.Y)
		size := image.Pt(max(inner.Size.X, gtx.Constraints.Min.X), h)
		size.X = min(size.X, max(gtx.Constraints.Max.X, inner.Size.X))
		alpha := uint8(255)
		if s.disabled {
			alpha = 140
		}
		opacity := paint.PushOpacity(gtx.Ops, float32(alpha)/255)
		fill(gtx, bg, size, 7)
		if edge.A != 0 && s.kind != primary {
			outline(gtx, edge, size, 7, 1)
		}
		offset := op.Offset(image.Pt((size.X-inner.Size.X)/2, (h-inner.Size.Y)/2)).Push(gtx.Ops)
		call.Add(gtx.Ops)
		offset.Pop()
		opacity.Pop()
		if gtx.Focused(c) {
			t.focusRing(gtx, size, 7)
		}
		return layout.Dimensions{Size: size}
	})
}

func mix(a, b color.NRGBA, amount float32) color.NRGBA {
	blend := func(x, y uint8) uint8 { return uint8(float32(x)*(1-amount) + float32(y)*amount) }
	return color.NRGBA{R: blend(a.R, b.R), G: blend(a.G, b.G), B: blend(a.B, b.B), A: 255}
}

// meter draws a rounded bar filled to ratio.
func (t *theme) meter(gtx layout.Context, ratio float32, c color.NRGBA, height unit.Dp) layout.Dimensions {
	w, h := gtx.Constraints.Max.X, gtx.Dp(height)
	paint.FillShape(gtx.Ops, t.track, clip.UniformRRect(image.Rect(0, 0, w, h), h/2).Op(gtx.Ops))
	if ratio > 0 {
		filled := max(h, int(float32(w)*min(1, ratio)))
		paint.FillShape(gtx.Ops, c, clip.UniformRRect(image.Rect(0, 0, filled, h), h/2).Op(gtx.Ops))
	}
	return layout.Dimensions{Size: image.Pt(w, h)}
}

// flow lays out children left to right and wraps them onto new lines when
// the row runs out of room.
type flow struct {
	gap, rowGap unit.Dp
	// end aligns each line to the right edge.
	end bool
	// middle centers children vertically within a line.
	middle bool
}

func (f flow) Layout(gtx layout.Context, children ...layout.Widget) layout.Dimensions {
	type placed struct {
		call op.CallOp
		size image.Point
	}
	maxW := gtx.Constraints.Max.X
	cgtx := gtx
	cgtx.Constraints.Min = image.Point{}
	items := make([]placed, 0, len(children))
	for _, child := range children {
		if child == nil {
			continue
		}
		record := op.Record(gtx.Ops)
		dims := child(cgtx)
		call := record.Stop()
		if dims.Size.X == 0 && dims.Size.Y == 0 {
			continue
		}
		items = append(items, placed{call, dims.Size})
	}
	gap, rowGap := gtx.Dp(f.gap), gtx.Dp(f.rowGap)
	var lines [][]placed
	var line []placed
	width := 0
	for _, item := range items {
		if len(line) > 0 && width+gap+item.size.X > maxW {
			lines = append(lines, line)
			line, width = nil, 0
		}
		if len(line) > 0 {
			width += gap
		}
		line = append(line, item)
		width += item.size.X
	}
	if len(line) > 0 {
		lines = append(lines, line)
	}
	y, total := 0, 0
	for i, line := range lines {
		lineW, lineH := 0, 0
		for j, item := range line {
			if j > 0 {
				lineW += gap
			}
			lineW += item.size.X
			lineH = max(lineH, item.size.Y)
		}
		x := 0
		if f.end {
			x = max(0, maxW-lineW)
		}
		for _, item := range line {
			dy := 0
			if f.middle {
				dy = (lineH - item.size.Y) / 2
			}
			stack := op.Offset(image.Pt(x, y+dy)).Push(gtx.Ops)
			item.call.Add(gtx.Ops)
			stack.Pop()
			x += item.size.X + gap
		}
		total = max(total, lineW)
		y += lineH
		if i < len(lines)-1 {
			y += rowGap
		}
	}
	if f.end {
		total = maxW
	}
	return layout.Dimensions{Size: image.Pt(max(total, gtx.Constraints.Min.X), y)}
}

// spread places left and right on one line when they fit, with right
// aligned to the end; otherwise it stacks right below left.
func spread(gtx layout.Context, gap unit.Dp, left, right layout.Widget) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	cgtx := gtx
	cgtx.Constraints.Min = image.Point{}
	record := op.Record(gtx.Ops)
	r := right(cgtx)
	rightCall := record.Stop()
	record = op.Record(gtx.Ops)
	lgtx := cgtx
	if r.Size.X > 0 && r.Size.X+gtx.Dp(gap) < maxW {
		lgtx.Constraints.Max.X = maxW - r.Size.X - gtx.Dp(gap)
	}
	l := left(lgtx)
	leftCall := record.Stop()
	if r.Size.X == 0 || l.Size.X+gtx.Dp(gap)+r.Size.X <= maxW {
		leftCall.Add(gtx.Ops)
		h := max(l.Size.Y, r.Size.Y)
		stack := op.Offset(image.Pt(maxW-r.Size.X, (h-r.Size.Y)/2)).Push(gtx.Ops)
		if l.Size.Y > r.Size.Y*2 {
			stack.Pop()
			stack = op.Offset(image.Pt(maxW-r.Size.X, 0)).Push(gtx.Ops)
		}
		rightCall.Add(gtx.Ops)
		stack.Pop()
		return layout.Dimensions{Size: image.Pt(maxW, h)}
	}
	// Not enough room side by side: lay left out again at full width.
	record = op.Record(gtx.Ops)
	l = left(cgtx)
	leftCall = record.Stop()
	leftCall.Add(gtx.Ops)
	y := l.Size.Y + gtx.Dp(gap)/2
	stack := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
	rightCall.Add(gtx.Ops)
	stack.Pop()
	return layout.Dimensions{Size: image.Pt(maxW, y+r.Size.Y)}
}

// split places left and right on one line when left fits at its natural
// width; otherwise right moves below left, so neither is truncated.
func split(gtx layout.Context, gap unit.Dp, left, right layout.Widget) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	cgtx := gtx
	cgtx.Constraints.Min = image.Point{}
	r := record(cgtx, right)
	l := record(cgtx, left)
	if r.size.X == 0 || l.size.X+gtx.Dp(gap)+r.size.X <= maxW {
		l.call.Add(gtx.Ops)
		h := max(l.size.Y, r.size.Y)
		dy := 0
		if l.size.Y < r.size.Y*2 {
			dy = (h - r.size.Y) / 2
		}
		stack := op.Offset(image.Pt(maxW-r.size.X, dy)).Push(gtx.Ops)
		r.call.Add(gtx.Ops)
		stack.Pop()
		return layout.Dimensions{Size: image.Pt(maxW, h)}
	}
	l.call.Add(gtx.Ops)
	y := l.size.Y + gtx.Dp(gap)/2
	stack := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
	r.call.Add(gtx.Ops)
	stack.Pop()
	return layout.Dimensions{Size: image.Pt(maxW, y+r.size.Y)}
}

// selectBox is a drop-down list in the concept's style. The closed control
// is sized to its longest option.
type selectBox struct {
	options  []string
	index    int
	open     bool
	disabled bool
	// prefix is shown before the value, for example "≥ ".
	prefix   string
	name     string
	button   widget.Clickable
	items    []widget.Clickable
	list     widget.List
	hot      int
	changed  bool
	scrim    bool // event tag for clicks outside the open list
	widthFor struct {
		options []string
		px      int
		metric  unit.Metric
	}
}

func (s *selectBox) setOptions(options []string, index int) {
	if !equalStrings(s.options, options) {
		s.options = append([]string(nil), options...)
	}
	s.index = clampIndex(index, len(options))
}

func (s *selectBox) value() string {
	if s.index >= 0 && s.index < len(s.options) {
		return s.options[s.index]
	}
	return ""
}

func (s *selectBox) choose(index int) {
	index = clampIndex(index, len(s.options))
	if index != s.index {
		s.index = index
		s.changed = true
	}
}

func clampIndex(index, n int) int {
	if n == 0 {
		return -1
	}
	return max(0, min(index, n-1))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Changed reports whether the user picked a new option since the last call.
func (s *selectBox) Changed() bool {
	changed := s.changed
	s.changed = false
	return changed
}

func (s *selectBox) update(gtx layout.Context) {
	if s.disabled {
		s.open = false
		return
	}
	for s.button.Clicked(gtx) {
		s.open = !s.open
		s.hot = s.index
		if s.open {
			s.list.Axis = layout.Vertical
			s.list.Position.First = max(0, s.index-3)
		}
	}
	for {
		e, ok := gtx.Event(
			key.Filter{Focus: &s.button, Name: key.NameUpArrow},
			key.Filter{Focus: &s.button, Name: key.NameDownArrow},
			key.Filter{Focus: &s.button, Name: key.NameEscape},
			key.Filter{Focus: &s.button, Name: key.NameHome},
			key.Filter{Focus: &s.button, Name: key.NameEnd},
		)
		if !ok {
			break
		}
		k, ok := e.(key.Event)
		if !ok || k.State != key.Press {
			continue
		}
		step := 0
		switch k.Name {
		case key.NameUpArrow:
			step = -1
		case key.NameDownArrow:
			step = 1
		case key.NameHome:
			step = -len(s.options)
		case key.NameEnd:
			step = len(s.options)
		case key.NameEscape:
			s.open = false
		}
		if step != 0 {
			if s.open {
				s.hot = clampIndex(s.hot+step, len(s.options))
			} else {
				s.choose(s.index + step)
			}
		}
	}
	for {
		e, ok := gtx.Event(key.Filter{Focus: &s.button, Name: key.NameReturn}, key.Filter{Focus: &s.button, Name: key.NameEnter})
		if !ok {
			break
		}
		if k, ok := e.(key.Event); ok && k.State == key.Press && s.open {
			s.choose(s.hot)
			s.open = false
		}
	}
	for {
		e, ok := gtx.Event(pointer.Filter{Target: &s.scrim, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := e.(pointer.Event); ok {
			s.open = false
		}
	}
	for i := range s.items {
		if s.items[i].Clicked(gtx) {
			s.choose(i)
			s.open = false
			gtx.Execute(key.FocusCmd{Tag: &s.button})
		}
	}
}

func (s *selectBox) Layout(gtx layout.Context, t *theme) layout.Dimensions {
	if len(s.items) != len(s.options) {
		s.items = make([]widget.Clickable, len(s.options))
	}
	s.update(gtx)
	if s.changed {
		// Views read Changed before this layout, so redraw to apply the choice.
		gtx.Execute(op.InvalidateCmd{})
	}
	body := style{size: 13, color: t.ink}
	padL, padR := gtx.Dp(10), gtx.Dp(32)
	if !equalStrings(s.widthFor.options, s.options) || s.widthFor.metric != gtx.Metric {
		// The open list shows the current option in semibold, so measure
		// that weight to keep every option whole.
		widest := 0
		bold := body
		bold.weight = font.SemiBold
		for _, option := range s.options {
			widest = max(widest, t.measure(gtx, bold, s.prefix+option))
		}
		s.widthFor.options, s.widthFor.px, s.widthFor.metric = s.options, widest, gtx.Metric
	}
	w := min(gtx.Constraints.Max.X, max(gtx.Constraints.Min.X, s.widthFor.px+padL+padR))
	h := gtx.Dp(34)
	cgtx := gtx
	if s.disabled {
		cgtx = gtx.Disabled()
	}
	dims := s.button.Layout(cgtx, func(gtx layout.Context) layout.Dimensions {
		size := image.Pt(w, h)
		bg := t.surface
		if s.button.Hovered() || s.open {
			bg = t.well
		}
		opacity := paint.PushOpacity(gtx.Ops, 1)
		if s.disabled {
			opacity.Pop()
			opacity = paint.PushOpacity(gtx.Ops, 0.55)
		}
		fill(gtx, bg, size, 7)
		edge := t.line
		if s.open {
			edge = t.accent
		}
		outline(gtx, edge, size, 7, 1)
		textGtx := gtx
		textGtx.Constraints = layout.Constraints{Max: image.Pt(max(0, w-padL-padR), h)}
		record := op.Record(gtx.Ops)
		d := t.label(textGtx, body, s.prefix+s.value())
		call := record.Stop()
		stack := op.Offset(image.Pt(padL, (h-d.Size.Y)/2)).Push(gtx.Ops)
		call.Add(gtx.Ops)
		stack.Pop()
		stack = op.Offset(image.Pt(w-gtx.Dp(26), (h-gtx.Dp(14))/2)).Push(gtx.Ops)
		drawIcon(gtx, iconChevronDown, 14, t.quiet)
		stack.Pop()
		opacity.Pop()
		if gtx.Focused(&s.button) {
			t.focusRing(gtx, size, 7)
		}
		return layout.Dimensions{Size: size}
	})
	if s.open {
		s.popup(gtx, t, dims.Size)
	}
	return dims
}

func (s *selectBox) popup(gtx layout.Context, t *theme, anchor image.Point) {
	record := op.Record(gtx.Ops)
	// Clicks anywhere outside the list close it.
	far := gtx.Dp(100000)
	scrim := clip.Rect{Min: image.Pt(-far, -far), Max: image.Pt(far, far)}.Push(gtx.Ops)
	event.Op(gtx.Ops, &s.scrim)
	scrim.Pop()
	rowH := gtx.Dp(32)
	visible := min(len(s.options), 9)
	pad := gtx.Dp(4)
	// Row padding, the check mark and the list inset around the widest option.
	width := max(anchor.X, s.widthFor.px+gtx.Dp(52))
	size := image.Pt(width, visible*rowH+2*pad)
	offset := op.Offset(image.Pt(0, anchor.Y+gtx.Dp(4))).Push(gtx.Ops)
	shadow := op.Offset(image.Pt(0, gtx.Dp(2))).Push(gtx.Ops)
	fill(gtx, color.NRGBA{A: 40}, size.Add(image.Pt(0, gtx.Dp(2))), 9)
	shadow.Pop()
	fill(gtx, t.surface, size, 8)
	outline(gtx, t.line, size, 8, 1)
	// Block clicks on the list from reaching the scrim.
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	event.Op(gtx.Ops, &s.items)
	area.Pop()
	inner := op.Offset(image.Pt(pad, pad)).Push(gtx.Ops)
	lgtx := gtx
	lgtx.Constraints = layout.Exact(image.Pt(width-2*pad, visible*rowH))
	s.list.Axis = layout.Vertical
	s.list.List.Layout(lgtx, len(s.options), func(gtx layout.Context, i int) layout.Dimensions {
		return s.items[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			row := image.Pt(gtx.Constraints.Max.X, rowH)
			if s.items[i].Hovered() {
				s.hot = i
			}
			switch {
			case i == s.hot:
				fill(gtx, t.well, row, 6)
			case i == s.index:
				fill(gtx, t.selected, row, 6)
			}
			c := t.ink
			weight := font.Normal
			if i == s.index {
				weight = font.SemiBold
			}
			tgtx := gtx
			tgtx.Constraints = layout.Constraints{Max: image.Pt(row.X-gtx.Dp(36), rowH)}
			rec := op.Record(gtx.Ops)
			d := t.label(tgtx, style{size: 13, weight: weight, color: c}, s.prefix+s.options[i])
			call := rec.Stop()
			st := op.Offset(image.Pt(gtx.Dp(10), (rowH-d.Size.Y)/2)).Push(gtx.Ops)
			call.Add(gtx.Ops)
			st.Pop()
			if i == s.index {
				st = op.Offset(image.Pt(row.X-gtx.Dp(24), (rowH-gtx.Dp(14))/2)).Push(gtx.Ops)
				drawIcon(gtx, iconCheck, 14, t.blue)
				st.Pop()
			}
			return layout.Dimensions{Size: row}
		})
	})
	inner.Pop()
	offset.Pop()
	op.Defer(gtx.Ops, record.Stop())
}

// searchField is a single-line editor with a search icon and a clear button.
type searchField struct {
	editor widget.Editor
	clear  widget.Clickable
	prompt string
	focus  bool // event tag for presses on the padding
}

func (s *searchField) text() string { return s.editor.Text() }

func (s *searchField) setText(value string) {
	if s.editor.Text() != value {
		s.editor.SetText(value)
	}
}

// Layout returns true when the text changed.
func (s *searchField) Layout(gtx layout.Context, t *theme, disabled bool) (layout.Dimensions, bool) {
	s.editor.SingleLine = true
	changed := false
	for {
		e, ok := s.editor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := e.(widget.ChangeEvent); ok {
			changed = true
		}
	}
	if s.clear.Clicked(gtx) {
		s.editor.SetText("")
		changed = true
		gtx.Execute(key.FocusCmd{Tag: &s.editor})
	}
	if changed {
		// The inspector may already be drawn for the old rows.
		gtx.Execute(op.InvalidateCmd{})
	}
	for {
		e, ok := gtx.Event(pointer.Filter{Target: &s.focus, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := e.(pointer.Event); ok {
			gtx.Execute(key.FocusCmd{Tag: &s.editor})
		}
	}
	if disabled {
		gtx = gtx.Disabled()
	}
	w, h := gtx.Constraints.Max.X, gtx.Dp(34)
	w = max(w, gtx.Constraints.Min.X)
	size := image.Pt(w, h)
	opacity := paint.PushOpacity(gtx.Ops, 1)
	if disabled {
		opacity.Pop()
		opacity = paint.PushOpacity(gtx.Ops, 0.55)
	}
	fill(gtx, t.surface, size, 7)
	focused := gtx.Focused(&s.editor)
	if focused {
		outline(gtx, t.accent, size, 7, 2)
	} else {
		outline(gtx, t.line, size, 7, 1)
	}
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	event.Op(gtx.Ops, &s.focus)
	pointer.CursorText.Add(gtx.Ops)
	area.Pop()
	pad := gtx.Dp(10)
	stack := op.Offset(image.Pt(pad, (h-gtx.Dp(16))/2)).Push(gtx.Ops)
	drawIcon(gtx, iconSearch, 16, t.quiet)
	stack.Pop()
	left := pad + gtx.Dp(16) + gtx.Dp(7)
	right := pad
	if s.editor.Len() > 0 {
		right = gtx.Dp(34)
	}
	egtx := gtx
	egtx.Constraints = layout.Constraints{Min: image.Pt(max(0, w-left-right), 0), Max: image.Pt(max(0, w-left-right), h)}
	body := style{size: 14, color: t.ink}
	record := op.Record(gtx.Ops)
	var d layout.Dimensions
	if s.editor.Len() == 0 {
		hint := op.Record(gtx.Ops)
		t.label(egtx, style{size: 14, color: t.quiet}, s.prompt)
		hintCall := hint.Stop()
		d = s.editorLayout(egtx, t, body)
		hintCall.Add(gtx.Ops)
	} else {
		d = s.editorLayout(egtx, t, body)
	}
	call := record.Stop()
	stack = op.Offset(image.Pt(left, (h-d.Size.Y)/2)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	stack.Pop()
	if s.editor.Len() > 0 {
		stack = op.Offset(image.Pt(w-gtx.Dp(30), (h-gtx.Dp(26))/2)).Push(gtx.Ops)
		s.clear.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			sz := image.Pt(gtx.Dp(26), gtx.Dp(26))
			if s.clear.Hovered() {
				fill(gtx, t.well, sz, 6)
			}
			st := op.Offset(image.Pt(gtx.Dp(6), gtx.Dp(6))).Push(gtx.Ops)
			drawIcon(gtx, iconClose, 14, t.quiet)
			st.Pop()
			return layout.Dimensions{Size: sz}
		})
		stack.Pop()
	}
	opacity.Pop()
	return layout.Dimensions{Size: size}, changed
}

func (s *searchField) editorLayout(gtx layout.Context, t *theme, body style) layout.Dimensions {
	text := op.Record(gtx.Ops)
	paint.ColorOp{Color: body.color}.Add(gtx.Ops)
	textMaterial := text.Stop()
	sel := op.Record(gtx.Ops)
	paint.ColorOp{Color: withAlpha(t.accent, 90)}.Add(gtx.Ops)
	selMaterial := sel.Stop()
	return s.editor.Layout(gtx, t.shaper, font.Font{Typeface: sans}, body.size, textMaterial, selMaterial)
}

// checkBox is a labeled check box.
type checkBox struct {
	value widget.Bool
}

func (c *checkBox) Layout(gtx layout.Context, t *theme, label string, disabled bool) (layout.Dimensions, bool) {
	changed := c.value.Update(gtx)
	if changed {
		gtx.Execute(op.InvalidateCmd{})
	}
	if disabled {
		gtx = gtx.Disabled()
	}
	dims := c.value.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		opacity := paint.PushOpacity(gtx.Ops, 1)
		if disabled {
			opacity.Pop()
			opacity = paint.PushOpacity(gtx.Ops, 0.55)
		}
		defer opacity.Pop()
		box := gtx.Dp(18)
		h := gtx.Dp(34)
		record := op.Record(gtx.Ops)
		lgtx := gtx
		lgtx.Constraints.Min = image.Point{}
		d := t.label(lgtx, style{size: 13, color: t.ink}, label)
		call := record.Stop()
		top := (h - box) / 2
		stack := op.Offset(image.Pt(0, top)).Push(gtx.Ops)
		if c.value.Value {
			fill(gtx, t.accent, image.Pt(box, box), 5)
			st := op.Offset(image.Pt(gtx.Dp(2), gtx.Dp(2))).Push(gtx.Ops)
			drawIcon(gtx, iconCheck, 14, t.onAccent)
			st.Pop()
		} else {
			fill(gtx, t.surface, image.Pt(box, box), 5)
			edge := t.line
			if c.value.Hovered() {
				edge = t.quiet
			}
			outline(gtx, edge, image.Pt(box, box), 5, 1)
		}
		stack.Pop()
		stack = op.Offset(image.Pt(box+gtx.Dp(8), (h-d.Size.Y)/2)).Push(gtx.Ops)
		call.Add(gtx.Ops)
		stack.Pop()
		size := image.Pt(box+gtx.Dp(8)+d.Size.X, h)
		if gtx.Focused(&c.value) {
			t.focusRing(gtx, size, 6)
		}
		return layout.Dimensions{Size: size}
	})
	return dims, changed
}

// fieldLabel is the small caption before a control, as in the concept's
// "Location" and "Compare" labels.
func (t *theme) fieldLabel(gtx layout.Context, txt string) layout.Dimensions {
	return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return t.label(gtx, style{size: 12, color: t.quiet}, txt)
	})
}

// labeled lays out a caption and a control on one line.
func labeled(gtx layout.Context, t *theme, caption string, control layout.Widget) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, style{size: 12, color: t.quiet}, caption)
		}),
		layout.Rigid(space(7, 0)),
		layout.Rigid(control),
	)
}

// centered lays out a widget in the middle of the available width.
func centered(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return layout.Center.Layout(gtx, w)
}

var _ = text.Start
