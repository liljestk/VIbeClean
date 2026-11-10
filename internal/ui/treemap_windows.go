package ui

import (
	"image"
	"slices"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const maxMapTiles = 32

type tileWeight struct {
	row   int
	bytes int64
	count int
}

type tileControl struct {
	key   model.RowKey
	click widget.Clickable
}

type fileTreemap struct {
	open   bool
	toggle widget.Clickable
	zoom   widget.Clickable
	tiles  [maxMapTiles]tileControl
}

// treemapWeights groups the smallest entries into an Other tile. Row numbers
// still refer to the displayed table, regardless of its sort order.
func treemapWeights(p *rankedPane, limit int, minimumShare float64) ([]tileWeight, int64) {
	weights := make([]tileWeight, 0, len(p.rows))
	var total int64
	for index, row := range p.rows {
		bytes := p.items[row].Item.Bytes
		if bytes > 0 {
			weights = append(weights, tileWeight{row: index, bytes: bytes, count: 1})
			total += bytes
		}
	}
	slices.SortStableFunc(weights, func(a, b tileWeight) int {
		switch {
		case a.bytes > b.bytes:
			return -1
		case a.bytes < b.bytes:
			return 1
		}
		return 0
	})
	individual := len(weights)
	if individual > limit {
		individual = limit - 1
	}
	for individual > 1 && float64(weights[individual-1].bytes) < float64(total)*minimumShare {
		individual--
	}
	if individual < len(weights) {
		other := tileWeight{row: -1, count: len(weights) - individual}
		for _, tile := range weights[individual:] {
			other.bytes += tile.bytes
		}
		weights = append(weights[:individual], other)
	}
	return weights, total
}

// arrangeTiles splits the longer dimension near the halfway weight. The
// resulting rectangles cover the map without gaps before drawing gutters.
func arrangeTiles(bounds image.Rectangle, weights []tileWeight, total int64, rects []image.Rectangle) {
	if len(weights) == 1 || bounds.Empty() {
		rects[0] = bounds
		return
	}
	size := bounds.Size()
	if size.X == 1 && size.Y == 1 {
		rects[0] = bounds
		return
	}
	split, weight := 1, weights[0].bytes
	for split < len(weights)-1 && weight+weights[split].bytes/2 < total/2 {
		weight += weights[split].bytes
		split++
	}
	a, b := bounds, bounds
	ratio := float64(weight) / float64(total)
	if size.X >= size.Y {
		a.Max.X = bounds.Min.X + min(size.X-1, max(1, int(float64(size.X)*ratio)))
		b.Min.X = a.Max.X
	} else {
		a.Max.Y = bounds.Min.Y + min(size.Y-1, max(1, int(float64(size.Y)*ratio)))
		b.Min.Y = a.Max.Y
	}
	arrangeTiles(a, weights[:split], weight, rects[:split])
	arrangeTiles(b, weights[split:], total-weight, rects[split:])
}

func (u *ui) layoutFileMap(gtx layout.Context) layout.Dimensions {
	t, p := u.th, &u.filePane
	if u.fileMap.zoom.Clicked(gtx) && p.canInspect() {
		u.inspectFolder()
		if u.details != nil {
			u.closeDetails(gtx)
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}
	}
	width := max(1, gtx.Constraints.Max.X)
	limit := max(4, min(maxMapTiles, width/max(1, gtx.Dp(34))))
	weights, total := treemapWeights(p, limit, min(0.08, float64(gtx.Dp(48))/float64(width)))
	label := units.Size(total) + " on this page"
	if len(weights) != 0 && weights[len(weights)-1].row < 0 {
		label += " · " + itemCount(weights[len(weights)-1].count, true) + " grouped"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.small(t.quiet), label)
		}),
		layout.Rigid(space(0, 8)),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			size := gtx.Constraints.Max
			if len(weights) == 0 {
				return t.label(gtx, t.small(t.quiet), "Items with allocated space appear here.")
			}
			var rects [maxMapTiles]image.Rectangle
			arrangeTiles(image.Rectangle{Max: size}, weights, total, rects[:len(weights)])
			for i, tile := range weights {
				rect := rects[i]
				if rect.Empty() {
					continue
				}
				offset := op.Offset(rect.Min).Push(gtx.Ops)
				u.layoutMapTile(gtx, &u.fileMap.tiles[i], tile, rect.Size())
				offset.Pop()
			}
			return layout.Dimensions{Size: size}
		}),
		layout.Rigid(space(0, 8)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = 0
			return t.button(gtx, &u.fileMap.zoom, buttonStyle{icon: iconInspect, label: "Inspect folder",
				compact: true, disabled: !p.canInspect()})
		}),
	)
}

func (u *ui) layoutMapTile(gtx layout.Context, control *tileControl, tile tileWeight, size image.Point) {
	t, p := u.th, &u.filePane
	name := "Other · " + itemCount(tile.count, true)
	color := t.quiet
	selected := tile.row >= 0 && tile.row == p.current
	var key model.RowKey
	if tile.row >= 0 {
		item := p.items[p.rows[tile.row]].Item
		key, name, color = model.RowKey{Series: item.Series, Key: item.Key}, item.Name, t.blue
		if !item.Directory {
			color = t.free
		}
	}
	if control.click.Clicked(gtx) && tile.row >= 0 && control.key == key {
		p.setCurrent(tile.row)
		selected = true
		gtx.Execute(op.InvalidateCmd{})
	}
	control.key = key
	size.X, size.Y = max(1, size.X-1), max(1, size.Y-1)
	gtx.Constraints = layout.Exact(size)
	draw := func(gtx layout.Context) layout.Dimensions {
		bg, ink := t.chrome, t.ink
		switch {
		case selected:
			bg = t.selected
			if t.contrast {
				ink = t.onAccent
			}
		case control.click.Hovered():
			bg = t.well
		}
		fill(gtx, bg, size, 3)
		fill(gtx, color, image.Pt(min(gtx.Dp(3), size.X), size.Y), 1)
		if selected || gtx.Focused(&control.click) {
			outline(gtx, t.accent, size, 3, 2)
		}
		if size.X >= gtx.Dp(44) && size.Y >= gtx.Dp(20) {
			bounds := clip.Rect{Max: size}.Push(gtx.Ops)
			layout.Inset{Left: 7, Right: 5, Top: 3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := name
				if size.Y >= gtx.Dp(42) {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(12, ink), label) }),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.small(ink), units.Size(tile.bytes)) }),
					)
				}
				return t.label(gtx, t.strong(12, ink), label)
			})
			bounds.Pop()
		}
		return layout.Dimensions{Size: size}
	}
	if tile.row < 0 {
		draw(gtx)
		return
	}
	control.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		defer t.exposeButton(gtx, size, &control.click, name+" · "+units.Size(tile.bytes)).Pop()
		return draw(gtx)
	})
}
