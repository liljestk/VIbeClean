package ui

import (
	"image"
	"image/color"
	"math"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
)

// icon draws a line icon on a 24-unit grid, in the style of the concept's
// Lucide icons, scaled to the requested size.
type icon func(p *pen)

// pen draws strokes in 24-unit icon coordinates.
type pen struct {
	ops   *op.Ops
	scale float32
	width float32
	color color.NRGBA
}

func (p *pen) pt(x, y float32) f32.Point { return f32.Pt(x*p.scale, y*p.scale) }

func (p *pen) stroke(spec clip.PathSpec) {
	paint.FillShape(p.ops, p.color, clip.Stroke{Path: spec, Width: p.width}.Op())
}

// poly strokes connected line segments through the given x, y pairs.
func (p *pen) poly(closed bool, xy ...float32) {
	var path clip.Path
	path.Begin(p.ops)
	path.MoveTo(p.pt(xy[0], xy[1]))
	for i := 2; i+1 < len(xy); i += 2 {
		path.LineTo(p.pt(xy[i], xy[i+1]))
	}
	if closed {
		path.Close()
	}
	p.stroke(path.End())
}

// rounded strokes a path made of straight segments whose corners are
// rounded with radius r.
func (p *pen) rounded(r float32, closed bool, xy ...float32) {
	n := len(xy) / 2
	point := func(i int) f32.Point { return f32.Pt(xy[2*i], xy[2*i+1]) }
	var path clip.Path
	path.Begin(p.ops)
	toward := func(from, to f32.Point, d float32) f32.Point {
		dx, dy := to.X-from.X, to.Y-from.Y
		length := float32(math.Hypot(float64(dx), float64(dy)))
		if length == 0 {
			return from
		}
		d = min(d, length/2)
		return f32.Pt(from.X+dx/length*d, from.Y+dy/length*d)
	}
	scaled := func(q f32.Point) f32.Point { return p.pt(q.X, q.Y) }
	start := 0
	if closed {
		path.MoveTo(scaled(toward(point(0), point(1), r)))
		start = 1
	} else {
		path.MoveTo(scaled(point(0)))
		start = 1
	}
	last := n - 1
	if closed {
		last = n
	}
	for i := start; i <= last; i++ {
		corner := point(i % n)
		prev := point((i - 1 + n) % n)
		if !closed && i == n-1 {
			path.LineTo(scaled(corner))
			break
		}
		next := point((i + 1) % n)
		path.LineTo(scaled(toward(corner, prev, r)))
		path.QuadTo(scaled(corner), scaled(toward(corner, next, r)))
	}
	if closed {
		path.Close()
	}
	p.stroke(path.End())
}

func (p *pen) circle(cx, cy, r float32) {
	rect := image.Rectangle{
		Min: image.Pt(int(math.Round(float64((cx-r)*p.scale))), int(math.Round(float64((cy-r)*p.scale)))),
		Max: image.Pt(int(math.Round(float64((cx+r)*p.scale))), int(math.Round(float64((cy+r)*p.scale)))),
	}
	p.stroke(clip.Ellipse(rect).Path(p.ops))
}

func (p *pen) dot(cx, cy, r float32) {
	rect := image.Rectangle{
		Min: image.Pt(int((cx-r)*p.scale), int((cy-r)*p.scale)),
		Max: image.Pt(int((cx+r)*p.scale+0.5), int((cy+r)*p.scale+0.5)),
	}
	paint.FillShape(p.ops, p.color, clip.Ellipse(rect).Op(p.ops))
}

// arc strokes part of a circle, from start through sweep radians,
// counter-clockwise for positive sweeps.
func (p *pen) arc(cx, cy, r, start, sweep float32) {
	var path clip.Path
	path.Begin(p.ops)
	from := p.pt(cx+r*float32(math.Cos(float64(start))), cy-r*float32(math.Sin(float64(start))))
	path.MoveTo(from)
	center := p.pt(cx, cy).Sub(from)
	path.Arc(center, center, -sweep)
	p.stroke(path.End())
}

// drawIcon lays out an icon in a size × size box.
func drawIcon(gtx layout.Context, ic icon, size unit.Dp, c color.NRGBA) layout.Dimensions {
	px := gtx.Dp(size)
	if ic != nil {
		scale := float32(px) / 24
		ic(&pen{ops: gtx.Ops, scale: scale, width: max(1, 1.85*scale), color: c})
	}
	return layout.Dimensions{Size: image.Pt(px, px)}
}

var (
	iconActivity icon = func(p *pen) { p.poly(false, 22, 12, 18, 12, 15, 21, 9, 3, 6, 12, 2, 12) }
	iconFolder   icon = func(p *pen) {
		p.rounded(2, true, 2, 4, 9, 4, 11, 6.5, 22, 6.5, 22, 20, 2, 20)
	}
	iconFile icon = func(p *pen) {
		p.poly(true, 5, 2, 14, 2, 20, 8, 20, 22, 5, 22)
		p.poly(false, 14, 2, 14, 8, 20, 8)
	}
	iconBoxes icon = func(p *pen) {
		p.poly(true, 12, 2, 21, 7, 21, 17, 12, 22, 3, 17, 3, 7)
		p.poly(false, 3, 7, 12, 12, 21, 7)
		p.poly(false, 12, 12, 12, 22)
	}
	iconPlug icon = func(p *pen) {
		p.poly(false, 12, 22, 12, 17)
		p.poly(false, 9, 8, 9, 2)
		p.poly(false, 15, 8, 15, 2)
		p.rounded(4, true, 6, 8, 18, 8, 18, 17, 6, 17)
	}
	iconSearch icon = func(p *pen) {
		p.circle(11, 11, 7)
		p.poly(false, 21, 21, 16.2, 16.2)
	}
	iconPanel icon = func(p *pen) {
		p.rounded(2.5, true, 3, 3, 21, 3, 21, 21, 3, 21)
		p.poly(false, 15, 3, 15, 21)
	}
	iconScan icon = func(p *pen) {
		p.rounded(2, false, 3, 7, 3, 3, 7, 3)
		p.rounded(2, false, 17, 3, 21, 3, 21, 7)
		p.rounded(2, false, 21, 17, 21, 21, 17, 21)
		p.rounded(2, false, 7, 21, 3, 21, 3, 17)
		p.poly(false, 7, 12, 17, 12)
	}
	iconRefresh icon = func(p *pen) {
		p.arc(12, 12, 8, math.Pi*0.25, math.Pi*1.6)
		p.poly(false, 20.5, 3.5, 20.5, 8.2, 15.8, 8.2)
	}
	iconCopy icon = func(p *pen) {
		p.rounded(2, true, 8, 8, 21, 8, 21, 21, 8, 21)
		p.rounded(2, false, 4, 16, 3, 16, 3, 3, 16, 3, 16, 4)
	}
	iconInspect icon = func(p *pen) {
		p.circle(11, 11, 7)
		p.poly(false, 21, 21, 16.2, 16.2)
		p.poly(false, 11, 8, 11, 14)
		p.poly(false, 8, 11, 14, 11)
	}
	iconChevronDown  icon = func(p *pen) { p.poly(false, 6, 9, 12, 15, 18, 9) }
	iconChevronLeft  icon = func(p *pen) { p.poly(false, 15, 18, 9, 12, 15, 6) }
	iconChevronRight icon = func(p *pen) { p.poly(false, 9, 18, 15, 12, 9, 6) }
	iconClose        icon = func(p *pen) {
		p.poly(false, 18, 6, 6, 18)
		p.poly(false, 6, 6, 18, 18)
	}
	iconMessage icon = func(p *pen) {
		p.rounded(2.5, true, 3, 4, 21, 4, 21, 17, 8, 17, 3, 21)
	}
	iconDrive icon = func(p *pen) {
		p.rounded(2, true, 2, 12, 22, 12, 22, 20, 2, 20)
		p.poly(false, 2, 12, 5.5, 5, 18.5, 5, 22, 12)
		p.dot(6.5, 16, 1.1)
		p.dot(10, 16, 1.1)
	}
	iconSave icon = func(p *pen) {
		p.poly(false, 12, 3, 12, 15)
		p.poly(false, 7, 10, 12, 15, 17, 10)
		p.poly(false, 5, 21, 19, 21)
	}
	iconExternal icon = func(p *pen) {
		p.poly(false, 15, 3, 21, 3, 21, 9)
		p.poly(false, 10, 14, 21, 3)
		p.rounded(2, false, 18, 13, 18, 21, 3, 21, 3, 6, 11, 6)
	}
	iconTerminal icon = func(p *pen) {
		p.poly(false, 4, 17, 10, 11, 4, 5)
		p.poly(false, 12, 19, 20, 19)
	}
	iconCode icon = func(p *pen) {
		p.poly(false, 16, 18, 22, 12, 16, 6)
		p.poly(false, 8, 6, 2, 12, 8, 18)
	}
	iconHistory icon = func(p *pen) {
		p.circle(12, 12, 9)
		p.poly(false, 12, 7, 12, 12, 15.5, 14)
	}
	iconCheck   icon = func(p *pen) { p.poly(false, 20, 6, 9, 17, 4, 12) }
	iconDiagnos icon = func(p *pen) {
		p.rounded(2, true, 4, 3, 20, 3, 20, 21, 4, 21)
		p.poly(false, 8, 8, 16, 8)
		p.poly(false, 8, 12, 16, 12)
		p.poly(false, 8, 16, 12, 16)
	}
)
