package ui

import (
	"cmp"
	"context"
	"image"
	"image/color"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gioui.org/font"
	"gioui.org/io/clipboard"
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
	"gioui.org/widget/material"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
)

// fact is one labeled reading shown in the inspector.
type fact struct{ label, value string }

// displayRow is one prepared table row. Rank orders the bars; Before and
// After hold sizes for sorting, or -1 when the column has no size.
type displayRow struct {
	Item   model.Record
	Cells  [4]string
	Rank   int64
	Delta  int64
	Before int64
	After  int64
	// Detail is searchable text and the inspector's note.
	Detail string
	// Facts are the inspector readings below the amount.
	Facts []fact
	// Mark is the removal level shown as a colored dot in the marked column.
	Mark model.Removal
}

// markWidth is the room a removal mark takes before a cell's text.
const markWidth unit.Dp = 15

// column describes one table column. Columns with a higher drop value are
// hidden first when the table is narrow; zero never drops.
type column struct {
	title string
	end   bool
	drop  int
}

// rankedPane is a sortable table of prepared rows with an inspector for the
// selected row and a chart of the largest visible rows.
type rankedPane struct {
	ctx     context.Context
	columns [4]column
	// changes colors the amount column by direction.
	changes bool
	// marked is the column that starts with each row's removal mark, or 0 for none.
	marked  int
	items   []displayRow
	rows    []int
	sortCol int
	sortAsc bool
	sorted  bool
	current int
	empty   string
	// emptyText is shown when there are no rows at all.
	emptyText string
	// Files keeps missing paths out of the displayed rows.
	missingPaths map[string]bool

	list    widget.List
	clicks  map[int]*widget.Clickable
	headers [4]widget.Clickable
	version int
	widths  struct {
		version int
		metric  unit.Metric
		need    [4]int
	}
	longest [4]string

	// Inspector actions.
	open, copy, inspect widget.Clickable
	cleanupAction       widget.Clickable
	cleanup             bool
	fileIcons           bool
	copied              bool
	openText            string
	inspectText         string
	inspectIcon         icon
	projectAction       bool
	inspectSeries       string
	inspectReady        bool
	inspectBlocked      bool
	rankTitle           string
	rankLinks           map[model.RowKey]*widget.Clickable
	details             widget.List
	onOpen, onInspect   func()
	onSort              func()
}

func (p *rankedPane) init(columns [4]column, changes bool, rankTitle string) {
	p.columns, p.changes, p.rankTitle = columns, changes, rankTitle
	p.current = -1
	p.list.Axis = layout.Vertical
	p.details.Axis = layout.Vertical
	p.clicks = make(map[int]*widget.Clickable)
	if changes {
		p.rankLinks = make(map[model.RowKey]*widget.Clickable)
	}
	p.sortCol = 1
}

func (p *rankedPane) selected() (displayRow, bool) {
	if p.current >= 0 && p.current < len(p.rows) {
		return p.items[p.rows[p.current]], true
	}
	return displayRow{}, false
}

func (p *rankedPane) selectedKey() (model.RowKey, bool) {
	row, ok := p.selected()
	return model.RowKey{Series: row.Item.Series, Key: row.Item.Key}, ok
}

func (p *rankedPane) restoreSelection(key model.RowKey) bool {
	for index, source := range p.rows {
		row := &p.items[source]
		if row.Item.Series == key.Series && row.Item.Key == key.Key {
			p.setCurrent(index)
			return true
		}
	}
	return false
}

func (p *rankedPane) setCurrent(index int) {
	if len(p.rows) == 0 {
		index = -1
	} else {
		index = max(0, min(index, len(p.rows)-1))
	}
	if index != p.current {
		p.copied = false
	}
	p.current = index
	p.reveal()
}

// reveal scrolls the list so the current row is visible.
func (p *rankedPane) reveal() {
	if p.current < 0 {
		return
	}
	pos := &p.list.Position
	visible := max(1, pos.Count-1)
	switch {
	case p.current < pos.First:
		pos.First, pos.Offset = p.current, 0
	case p.current >= pos.First+visible:
		pos.First, pos.Offset = max(0, p.current-visible+1), 0
	}
}

// show replaces the rows, keeping the selected item when it is still listed.
func (p *rankedPane) show(rows []displayRow, query, empty string) {
	key, keep := p.selectedKey()
	p.items, p.emptyText = rows, empty
	p.version++
	for c := range p.longest {
		p.longest[c] = ""
	}
	for _, row := range rows {
		for c := 1; c < len(row.Cells); c++ {
			if len([]rune(row.Cells[c])) > len([]rune(p.longest[c])) {
				p.longest[c] = row.Cells[c]
			}
		}
	}
	p.applyFilter(query, key, keep)
}

func (p *rankedPane) search(query, view string) {
	if len(p.items) == 0 {
		return
	}
	started := time.Now()
	key, keep := p.selectedKey()
	p.applyFilter(query, key, keep)
	logRender(p.ctx, view+".search", started, len(p.items), len(p.rows))
}

func (p *rankedPane) applyFilter(query string, key model.RowKey, keep bool) {
	filter := model.ParseFilter(query)
	rows := p.rows[:0]
	for index := range p.items {
		row := &p.items[index]
		if !p.missingPaths[row.Item.Path] && filter.Matches(row.Item, row.Detail) {
			rows = append(rows, index)
		}
	}
	p.rows = rows
	p.empty = p.emptyText
	if len(rows) == 0 && len(p.items) > 0 {
		p.empty = "No matches. Try a different search."
		if len(p.missingPaths) > 0 {
			p.empty = "No current items match."
		}
	}
	if !p.sorted {
		p.sortCol, p.sortAsc, p.sorted = 1, false, true
	}
	p.sortRows()
	p.current = -1
	index := 0
	if keep {
		for i, source := range p.rows {
			row := &p.items[source]
			if row.Item.Series == key.Series && row.Item.Key == key.Key {
				index = i
				break
			}
		}
	}
	p.setCurrent(index)
	p.copied = false
}

func (p *rankedPane) sortRows() {
	if p.onSort != nil {
		// Inventory pages arrive in the backend's global sort order.
		return
	}
	column, ascending := p.sortCol, p.sortAsc
	slices.SortFunc(p.rows, func(left, right int) int {
		ordering := compareColumn(&p.items[left], &p.items[right], column)
		if ordering == 0 {
			if byName := strings.Compare(p.items[left].Item.Name, p.items[right].Item.Name); byName != 0 {
				return byName
			}
			return cmp.Compare(left, right)
		}
		if !ascending {
			return -ordering
		}
		return ordering
	})
}

func (p *rankedPane) sortBy(column int) {
	p.version++
	key, keep := p.selectedKey()
	if p.sortCol == column {
		p.sortAsc = !p.sortAsc
	} else {
		// Names and text columns read best A to Z, and removal levels from the
		// easiest win; sizes largest first.
		title := p.columns[column].title
		p.sortCol, p.sortAsc = column, column == 0 || title == "Category" || title == "Type" ||
			title == "Activity" || title == "Removal"
	}
	p.sortRows()
	p.current = -1
	if keep {
		p.restoreSelection(key)
	}
	if p.current < 0 {
		p.setCurrent(0)
	}
	if p.onSort != nil {
		p.onSort()
	}
}

func compareColumn(a, b *displayRow, column int) int {
	switch {
	case column == 1:
		return cmp.Compare(a.Rank, b.Rank)
	case column == 2 && (a.Before >= 0 || b.Before >= 0):
		return cmp.Compare(a.Before, b.Before)
	case column == 3 && (a.After >= 0 || b.After >= 0):
		return cmp.Compare(a.After, b.After)
	}
	return strings.Compare(strings.ToLower(a.Cells[column]), strings.ToLower(b.Cells[column]))
}

// largest fills ranked with the visible rows with the largest known sizes.
func largest(items []displayRow, rows, ranked []int) []int {
	count := 0
	for _, source := range rows {
		rank := items[source].Rank
		if rank <= 0 {
			continue
		}
		slot := count
		for slot > 0 && items[ranked[slot-1]].Rank < rank {
			slot--
		}
		if slot < len(ranked) {
			count = min(count+1, len(ranked))
			copy(ranked[slot+1:count], ranked[slot:count-1])
			ranked[slot] = source
		}
	}
	return ranked[:count]
}

func logRender(ctx context.Context, view string, started time.Time, input, shown int) {
	elapsed := time.Since(started)
	level := slog.LevelDebug
	if elapsed >= slowRender {
		level = slog.LevelWarn
	}
	diag.Log(ctx, level, "gui.render", slog.String("view", view),
		slog.Float64("duration_ms", diag.Milliseconds(elapsed)), slog.Int("input_rows", input), slog.Int("visible_rows", shown))
}

// activate runs the primary action for the selected row.
func (p *rankedPane) activate() {
	if p.canOpen() && p.onOpen != nil {
		p.onOpen()
	}
}

func (p *rankedPane) canOpen() bool {
	row, ok := p.selected()
	return ok && ((!p.projectAction && row.Item.Path != "") || (p.projectAction && row.Item.Series == model.Projects))
}

func (p *rankedPane) canInspect() bool {
	row, ok := p.selected()
	return ok && p.onInspect != nil && row.Item.Directory && !p.inspectBlocked &&
		(p.inspectSeries == "" || (p.inspectReady && row.Item.Series == p.inspectSeries))
}

func (p *rankedPane) copyValue(gtx layout.Context) {
	row, ok := p.selected()
	if !ok {
		return
	}
	value := row.Item.Path
	if value == "" {
		value = row.Item.Name
	}
	gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(value))})
	p.copied = true
}

// handleKeys moves the selection with the keyboard while the table has focus.
func (p *rankedPane) handleKeys(gtx layout.Context) {
	page := max(1, p.list.Position.Count-1)
	for {
		e, ok := gtx.Event(
			key.FocusFilter{Target: p},
			key.Filter{Focus: p, Name: key.NameUpArrow},
			key.Filter{Focus: p, Name: key.NameDownArrow},
			key.Filter{Focus: p, Name: key.NamePageUp},
			key.Filter{Focus: p, Name: key.NamePageDown},
			key.Filter{Focus: p, Name: key.NameHome},
			key.Filter{Focus: p, Name: key.NameEnd},
			key.Filter{Focus: p, Name: key.NameReturn},
			key.Filter{Focus: p, Name: key.NameEnter},
			key.Filter{Focus: p, Name: "C", Required: key.ModShortcut},
		)
		if !ok {
			break
		}
		k, ok := e.(key.Event)
		if !ok || k.State != key.Press {
			continue
		}
		switch k.Name {
		case key.NameUpArrow:
			p.setCurrent(p.current - 1)
		case key.NameDownArrow:
			p.setCurrent(p.current + 1)
		case key.NamePageUp:
			p.setCurrent(p.current - page)
		case key.NamePageDown:
			p.setCurrent(p.current + page)
		case key.NameHome:
			p.setCurrent(0)
		case key.NameEnd:
			p.setCurrent(len(p.rows) - 1)
		case key.NameReturn, key.NameEnter:
			p.activate()
		case "C":
			p.copyValue(gtx)
		}
	}
}

// visibleColumns decides which columns fit and how wide each one is. The
// name column takes the remaining width; the others get their measured
// width so values are never cut off while a column is shown.
func (p *rankedPane) visibleColumns(gtx layout.Context, t *theme, width int) (shown [4]bool, widths [4]int) {
	if p.widths.version != p.version || p.widths.metric != gtx.Metric {
		header := style{size: 12, weight: font.SemiBold, color: t.quiet}
		for c := 1; c < len(p.columns); c++ {
			need := t.measure(gtx, header, p.columns[c].title+" ↓")
			cell := style{size: 14, color: t.ink}
			if c == 1 && p.changes {
				cell.weight = font.SemiBold
			}
			if p.longest[c] != "" {
				need = max(need, t.measure(gtx, cell, p.longest[c]))
			}
			if c == p.marked {
				need += gtx.Dp(markWidth)
			}
			p.widths.need[c] = need + gtx.Dp(26)
		}
		p.widths.version, p.widths.metric = p.version, gtx.Metric
	}
	shown = [4]bool{true, true, true, true}
	nameMin := gtx.Dp(150)
	for {
		total := nameMin
		for c := 1; c < len(p.columns); c++ {
			if shown[c] {
				total += p.widths.need[c]
			}
		}
		if total <= width {
			break
		}
		drop, worst := -1, 0
		for c := 1; c < len(p.columns); c++ {
			if shown[c] && p.columns[c].drop > worst {
				drop, worst = c, p.columns[c].drop
			}
		}
		if drop < 0 {
			break
		}
		shown[drop] = false
	}
	rest := width
	for c := 1; c < len(p.columns); c++ {
		if shown[c] {
			widths[c] = p.widths.need[c]
			rest -= widths[c]
		}
	}
	widths[0] = max(gtx.Dp(80), rest)
	return shown, widths
}

// layoutTable draws the header, the rows and the empty state.
func (p *rankedPane) layoutTable(gtx layout.Context, t *theme) layout.Dimensions {
	// The inspector is laid out before the table, so redraw when the
	// selection or order changes here.
	before, version := p.current, p.version
	defer func() {
		if p.current != before || p.version != version {
			gtx.Execute(op.InvalidateCmd{})
		}
	}()
	p.handleKeys(gtx)
	for c := range p.headers {
		if p.headers[c].Clicked(gtx) {
			p.sortBy(c)
		}
	}
	size := gtx.Constraints.Max
	headerH := gtx.Dp(32)
	if size.Y < headerH {
		return layout.Dimensions{Size: size}
	}
	gutter := gtx.Dp(10) // room for the overlay scrollbar
	shown, widths := p.visibleColumns(gtx, t, size.X-gutter)
	rowH := gtx.Dp(38)
	padX := gtx.Dp(12)
	focused := gtx.Focused(p)

	// Focus and keyboard area for the whole table.
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	event.Op(gtx.Ops, p)
	area.Pop()

	// Header.
	paint.FillShape(gtx.Ops, t.chrome, clip.RRect{Rect: image.Rect(0, 0, size.X, headerH),
		NW: gtx.Dp(6), NE: gtx.Dp(6)}.Op(gtx.Ops))
	x := 0
	for c := range p.columns {
		if !shown[c] {
			continue
		}
		w := widths[c]
		if c == len(p.columns)-1 {
			w += gutter
		}
		stack := op.Offset(image.Pt(x, 0)).Push(gtx.Ops)
		hgtx := gtx
		hgtx.Constraints = layout.Exact(image.Pt(w, headerH))
		p.headers[c].Layout(hgtx, func(gtx layout.Context) layout.Dimensions {
			title := p.columns[c].title
			if p.sortCol == c {
				if p.sortAsc {
					title += " ↑"
				} else {
					title += " ↓"
				}
			}
			defer t.exposeButton(gtx, image.Pt(w, headerH), &p.headers[c], "Sort by "+title).Pop()
			c2 := t.quiet
			if p.headers[c].Hovered() || p.sortCol == c {
				c2 = t.ink
			}
			s := style{size: 12, weight: font.SemiBold, color: c2}
			if p.columns[c].end {
				s.align = text.End
			}
			inner := w - 2*padX
			if c == len(p.columns)-1 {
				inner -= gutter
			}
			lgtx := gtx
			lgtx.Constraints = layout.Constraints{Min: image.Pt(max(0, inner), 0), Max: image.Pt(max(0, inner), headerH)}
			rec := op.Record(gtx.Ops)
			d := t.label(lgtx, s, title)
			call := rec.Stop()
			st := op.Offset(image.Pt(padX, (headerH-d.Size.Y)/2)).Push(gtx.Ops)
			call.Add(gtx.Ops)
			st.Pop()
			if gtx.Focused(&p.headers[c]) {
				t.focusRing(gtx, image.Pt(w, headerH), 6)
			}
			return layout.Dimensions{Size: image.Pt(w, headerH)}
		})
		stack.Pop()
		x += widths[c]
	}

	// Rows.
	listTop := headerH + gtx.Dp(2)
	stack := op.Offset(image.Pt(0, listTop)).Push(gtx.Ops)
	lgtx := gtx
	lgtx.Constraints = layout.Exact(image.Pt(size.X, max(0, size.Y-listTop)))
	if len(p.rows) == 0 {
		msg := p.empty
		if msg == "" {
			msg = "Nothing to show yet."
		}
		layout.Inset{Top: 18, Left: 12, Right: 12}.Layout(lgtx, func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.body(t.quiet).wrapped(3), msg)
		})
		stack.Pop()
		return layout.Dimensions{Size: size}
	}
	seen := make(map[int]bool, 32)
	list := material.List(&material.Theme{FingerSize: 38}, &p.list)
	list.AnchorStrategy = material.Overlay
	list.Track.MinorPadding = 2
	list.Indicator.MinorWidth = 6
	list.Indicator.CornerRadius = 3
	list.Indicator.MajorMinLen = 32
	list.Indicator.Color = withAlpha(t.quiet, 90)
	list.Indicator.HoverColor = withAlpha(t.quiet, 170)
	list.Layout(lgtx, len(p.rows), func(gtx layout.Context, index int) layout.Dimensions {
		seen[index] = true
		click := p.clicks[index]
		if click == nil {
			click = new(widget.Clickable)
			p.clicks[index] = click
		}
		for {
			ev, ok := click.Update(gtx)
			if !ok {
				break
			}
			p.setCurrent(index)
			gtx.Execute(key.FocusCmd{Tag: p})
			if ev.NumClicks >= 2 {
				p.activate()
			}
		}
		return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return p.layoutRow(gtx, t, index, shown, widths, rowH, padX, gutter, focused)
		})
	})
	for index := range p.clicks {
		if !seen[index] {
			delete(p.clicks, index)
		}
	}
	stack.Pop()
	return layout.Dimensions{Size: size}
}

func (p *rankedPane) layoutRow(gtx layout.Context, t *theme, index int, shown [4]bool, widths [4]int,
	rowH, padX, gutter int, focused bool) layout.Dimensions {
	row := &p.items[p.rows[index]]
	w := gtx.Constraints.Max.X
	size := image.Pt(w, rowH)
	if t.access != nil {
		key := model.RowKey{Series: row.Item.Series, Key: row.Item.Key}
		selectRow := func() { p.restoreSelection(key) }
		name := strings.Trim(strings.Join(row.Cells[:], ", "), ", ")
		if row.Mark != "" && p.changes {
			name += ", " + removalTitle(row.Mark)
		}
		if p.fileIcons && row.Item.Path != "" {
			if row.Item.Directory {
				name = "Folder, " + name
			} else {
				name = "File, " + name
			}
		}
		defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
		t.markAccess(gtx, accessKey{tag: p, item: key.Series + "\x00" + key.Key},
			accessNode{name: name, role: accessRow, tag: p, focused: focused && index == p.current,
				invoke: selectRow, beforeFocus: selectRow})
	}
	bg := t.surface
	switch {
	case index == p.current:
		bg = t.selected
	case p.clicks[index] != nil && p.clicks[index].Hovered():
		bg = t.well
	case index%2 == 1:
		bg = t.chrome
	}
	if bg != t.surface {
		fill(gtx, bg, image.Pt(w-gutter/2, rowH), 6)
	}
	if index == p.current && focused {
		outline(gtx, t.accent, image.Pt(w-gutter/2, rowH), 6, 1.5)
	}
	x := 0
	for c := range p.columns {
		if !shown[c] {
			continue
		}
		cw := widths[c]
		s := style{size: 14, color: t.ink}
		if index == p.current && t.contrast {
			s.color = t.onAccent
		}
		switch {
		case c == 0:
			s.weight = font.SemiBold
		case c == 1 && p.changes:
			s.weight = font.SemiBold
			if !t.contrast {
				s.color = t.tone(row.Delta)
			}
		}
		if p.columns[c].end {
			s.align = text.End
		}
		inner := max(0, cw-2*padX)
		textX := x + padX
		if c == 0 && p.fileIcons && row.Item.Path != "" {
			ic := iconFile
			if row.Item.Directory {
				ic = iconFolder
			}
			iconSize := gtx.Dp(14)
			stack := op.Offset(image.Pt(textX, (rowH-iconSize)/2)).Push(gtx.Ops)
			drawIcon(gtx, ic, 14, s.color)
			stack.Pop()
			textX += gtx.Dp(22)
			inner = max(0, inner-gtx.Dp(22))
		}
		if c == p.marked && row.Mark != "" {
			dot := gtx.Dp(8)
			mark := op.Offset(image.Pt(textX, (rowH-dot)/2)).Push(gtx.Ops)
			paint.FillShape(gtx.Ops, t.removal(row.Mark), clip.Ellipse{Max: image.Pt(dot, dot)}.Op(gtx.Ops))
			mark.Pop()
		}
		if c == p.marked {
			textX += gtx.Dp(markWidth)
			inner = max(0, inner-gtx.Dp(markWidth))
		}
		cgtx := gtx
		cgtx.Constraints = layout.Constraints{Min: image.Pt(inner, 0), Max: image.Pt(inner, rowH)}
		rec := op.Record(gtx.Ops)
		d := t.label(cgtx, s, row.Cells[c])
		call := rec.Stop()
		st := op.Offset(image.Pt(textX, (rowH-d.Size.Y)/2)).Push(gtx.Ops)
		call.Add(gtx.Ops)
		st.Pop()
		x += cw
	}
	return layout.Dimensions{Size: size}
}

// layoutInspector draws the selected row, its actions and the chart of the
// largest visible rows. Wide panes stack the sections; short ones put them
// side by side.
func (p *rankedPane) layoutInspector(gtx layout.Context, u *ui) layout.Dimensions {
	t := u.th
	for row, click := range p.rankLinks {
		if click.Clicked(gtx) && p.restoreSelection(row) {
			p.details.Position = layout.Position{}
			gtx.Execute(key.FocusCmd{Tag: p})
			gtx.Execute(op.InvalidateCmd{})
		}
	}
	if p.open.Clicked(gtx) && p.canOpen() && p.onOpen != nil {
		p.onOpen()
	}
	if p.copy.Clicked(gtx) {
		p.copyValue(gtx)
	}
	if p.inspect.Clicked(gtx) && p.canInspect() {
		p.onInspect()
		if u.details != nil {
			u.closeDetails(gtx)
		}
	}
	if p.cleanupAction.Clicked(gtx) && p.cleanup {
		if row, ok := p.selected(); ok {
			u.openCleanup(row.Item.Kind)
		}
	}
	if p == &u.filePane && u.deleteButton.Clicked(gtx) {
		if row, ok := p.selected(); ok {
			u.openRecycle(gtx, row.Item)
		}
	}
	size := gtx.Constraints.Max
	fill(gtx, t.chrome, size, 10)
	pad := layout.Inset{Top: 14, Bottom: 14, Left: 16, Right: 16}
	selection := func(gtx layout.Context) layout.Dimensions { return p.layoutSelection(gtx, u) }
	ranking := func(gtx layout.Context) layout.Dimensions { return p.layoutRanking(gtx, t) }
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = gtx.Constraints.Max
		if u.canShowMap() {
			return scrollColumn(gtx, &p.details, selection)
		}
		return scrollColumn(gtx, &p.details, selection, space(0, 22), ranking)
	})
}

// scrollColumn stacks widgets vertically and scrolls when they overflow.
func scrollColumn(gtx layout.Context, list *widget.List, children ...layout.Widget) layout.Dimensions {
	list.Axis = layout.Vertical
	return list.List.Layout(gtx, len(children), func(gtx layout.Context, i int) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return children[i](gtx)
	})
}

func (p *rankedPane) layoutSelection(gtx layout.Context, u *ui) layout.Dimensions {
	t := u.th
	row, ok := p.selected()
	title := func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return drawIcon(gtx, iconPanel, 15, t.quiet) }),
			layout.Rigid(space(7, 0)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(12, t.quiet), "Selected item")
			}),
		)
	}
	children := []layout.FlexChild{layout.Rigid(title), layout.Rigid(space(0, 12))}
	if !ok {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.body(t.quiet).wrapped(3), selectPrompt)
		}))
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	}
	amountColor := t.ink
	if p.changes && !t.contrast {
		amountColor = t.tone(row.Delta)
	}
	path := row.Item.Path
	if path == "" {
		path = row.Item.Kind
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.strong(17, t.ink).wrapped(2), row.Item.Name)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if path == "" || path == row.Item.Name {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				s := t.small(t.quiet).wrapped(3)
				s.wrap = text.WrapGraphemes
				return t.label(gtx, s, path)
			})
		}),
		layout.Rigid(space(0, 8)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.strong(24, amountColor), row.Cells[1])
		}),
	)
	if len(row.Facts) > 0 {
		children = append(children, layout.Rigid(space(0, 8)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			var items []layout.Widget
			for _, f := range row.Facts {
				items = append(items, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.small(t.quiet), f.label) }),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return t.label(gtx, style{size: 13, weight: font.SemiBold, color: t.ink}, f.value)
						}),
					)
				})
			}
			return flow{gap: 18, rowGap: 8}.Layout(gtx, items...)
		}))
	}
	if row.Detail != "" {
		children = append(children, layout.Rigid(space(0, 10)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.small(t.quiet).wrapped(-1), row.Detail)
		}))
	}
	children = append(children, layout.Rigid(space(0, 12)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		copyText := "Copy path"
		if row.Item.Path == "" {
			copyText = "Copy name"
		}
		if p.copied {
			copyText = "Copied"
		}
		var buttons []layout.Widget
		// A category total spans many folders, so only its name can be copied.
		folderActions := row.Item.Series != model.Categories
		if p.cleanup {
			if label, _ := cleanupShortcut(row.Item.Kind); label != "" {
				buttons = append(buttons, func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &p.cleanupAction, buttonStyle{icon: iconChevronRight, label: label, compact: true})
				})
			}
		}
		if p.inspectText != "" && folderActions {
			buttons = append(buttons, func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &p.inspect, buttonStyle{icon: p.inspectIcon, label: p.inspectText, disabled: !p.canInspect(), compact: true})
			})
		}
		if p.openText != "" && folderActions {
			buttons = append(buttons, func(gtx layout.Context) layout.Dimensions {
				ic, label := iconFolder, p.openText
				if p.projectAction {
					ic = iconChevronRight
				} else if !row.Item.Directory && row.Item.Path != "" {
					label = "Show in Explorer"
				}
				return t.button(gtx, &p.open, buttonStyle{icon: ic, label: label, disabled: !p.canOpen(), compact: true})
			})
		}
		buttons = append(buttons, func(gtx layout.Context) layout.Dimensions {
			ic := iconCopy
			if p.copied {
				ic = iconCheck
			}
			return t.button(gtx, &p.copy, buttonStyle{icon: ic, label: copyText, compact: true})
		})
		if p == &u.filePane && u.canOfferDelete(row.Item) {
			buttons = append(buttons, func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, &u.deleteButton, buttonStyle{label: "Delete…", compact: true, disabled: u.active != nil})
			})
		}
		return flow{gap: 6, rowGap: 6}.Layout(gtx, buttons...)
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (p *rankedPane) layoutRanking(gtx layout.Context, t *theme) layout.Dimensions {
	var buffer [5]int
	ranked := largest(p.items, p.rows, buffer[:])
	if len(ranked) == 0 {
		clear(p.rankLinks)
		return layout.Dimensions{}
	}
	var keys [5]model.RowKey
	top := float32(p.items[ranked[0]].Rank)
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(12, t.ink), p.rankTitle) }),
		layout.Rigid(space(0, 8)),
	}
	for i, source := range ranked {
		row := p.items[source]
		rowKey := model.RowKey{Series: row.Item.Series, Key: row.Item.Key}
		keys[i] = rowKey
		barColor := t.blue
		if p.changes && !t.contrast {
			barColor = t.tone(row.Delta)
		}
		if i > 0 {
			gap := unit.Dp(9)
			if p.changes {
				gap = 3
			}
			children = append(children, layout.Rigid(space(0, gap)))
		}
		bar := func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return spread(gtx, 8,
						func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.small(t.ink), row.Item.Name) },
						func(gtx layout.Context) layout.Dimensions {
							return t.label(gtx, style{size: 12, weight: font.SemiBold, color: t.ink}, row.Cells[1])
						})
				}),
				layout.Rigid(space(0, 4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return t.meter(gtx, float32(row.Rank)/top, barColor, 5)
				}),
			)
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !p.changes {
				return bar(gtx)
			}
			click := p.rankLinks[rowKey]
			if click == nil {
				click = new(widget.Clickable)
				p.rankLinks[rowKey] = click
			}
			return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				pointer.CursorPointer.Add(gtx.Ops)
				var bg color.NRGBA
				if click.Hovered() || click.Pressed() {
					bg = t.well
				}
				d := record(gtx, func(gtx layout.Context) layout.Dimensions {
					return t.panel(gtx, layout.Inset{Top: 4, Bottom: 4, Left: 4, Right: 4}, 6, bg, bar)
				})
				defer t.exposeButton(gtx, d.size, click, "Select "+row.Item.Name+", "+row.Cells[1]).Pop()
				d.call.Add(gtx.Ops)
				if gtx.Focused(click) {
					t.focusRing(gtx, d.size, 6)
				}
				return layout.Dimensions{Size: d.size}
			})
		}))
	}
	for key := range p.rankLinks {
		if !slices.Contains(keys[:len(ranked)], key) {
			delete(p.rankLinks, key)
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

var _ = pointer.CursorDefault
