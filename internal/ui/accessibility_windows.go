package ui

import (
	"image"
	"slices"
	"strconv"
	"sync/atomic"

	"gioui.org/io/event"
	"gioui.org/io/input"
	"gioui.org/io/key"
	"gioui.org/io/semantic"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/widget"
)

const (
	accessButton     = 50000
	accessCheck      = 50002
	accessEdit       = 50004
	accessRow        = 50007
	accessText       = 50020
	accessWindowRole = 50032
	accessHidden     = "hidden"
)

type accessKey struct {
	tag  event.Tag
	item string
}

type accessNode struct {
	name, value string
	role        int32
	bounds      image.Rectangle
	tag         event.Tag
	disabled    bool
	focused     bool
	checked     bool
	invoke      func()
	setValue    func(string)
	beforeFocus func()
}

type accessTree struct {
	nodes map[int32]accessNode
	order []int32
	focus int32
}

// accessFrame reuses Gio's semantic bounds and labels. Native callbacks read
// an immutable frame, and queue actions back to the UI goroutine.
type accessFrame struct {
	router    input.Router
	semantics []input.SemanticNode
	next      int32
	targets   map[int32]accessNode
	keys      map[accessKey]int32
	priorKeys map[accessKey]int32
	tree      atomic.Pointer[accessTree]
	focus     int32
}

func (a *accessFrame) begin(gtx layout.Context) {
	if a.focus != 0 {
		if tree := a.tree.Load(); tree != nil {
			if node := tree.nodes[a.focus]; node.tag != nil && !node.disabled {
				if node.beforeFocus != nil {
					node.beforeFocus()
				}
				gtx.Execute(key.FocusCmd{Tag: node.tag})
			}
		}
		a.focus = 0
	}
	a.keys, a.priorKeys = a.priorKeys, a.keys
	clear(a.keys)
	clear(a.targets)
}

func (t *theme) expose(gtx layout.Context, size image.Point, id accessKey, node accessNode) {
	if t.access == nil || size.X <= 0 || size.Y <= 0 {
		return
	}
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	t.markAccess(gtx, id, node)
	area.Pop()
}

func (t *theme) markAccess(gtx layout.Context, id accessKey, node accessNode) {
	a := t.access
	if a == nil || node.name == "" {
		return
	}
	key := a.priorKeys[id]
	if key == 0 {
		a.next++
		key = a.next
	}
	a.keys[id] = key
	node.disabled = !gtx.Enabled()
	if node.role != accessRow && node.tag != nil {
		node.focused = gtx.Focused(node.tag)
	}
	a.targets[key] = node
	semantic.DescriptionOp(strconv.FormatInt(int64(key), 10)).Add(gtx.Ops)
}

func (t *theme) exposeButton(gtx layout.Context, size image.Point, click *widget.Clickable, name string) clip.Stack {
	// Keep metadata in a separate area from the Gio input handler. The
	// semantic router has no pointer filters and drops tagged input areas.
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	t.markAccess(gtx, accessKey{tag: click}, accessNode{name: name, role: accessButton, tag: click, invoke: click.Click})
	return area
}

func (a *accessFrame) snapshot(ops *op.Ops, size image.Point) (changed bool, focus int32) {
	a.router.Frame(ops)
	a.semantics = a.router.AppendSemantics(a.semantics[:0])
	bounds := image.Rectangle{Max: size}
	tree := &accessTree{nodes: map[int32]accessNode{
		0: {name: "Space Review", role: accessWindowRole, bounds: bounds},
	}}
	tree.append(a.semantics[0], bounds, a.targets)
	previous := a.tree.Swap(tree)
	if previous == nil {
		return true, tree.focus
	}
	if previous.focus != tree.focus {
		focus = tree.focus
	}
	return !slices.Equal(previous.order, tree.order), focus
}

func (t *accessTree) append(sem input.SemanticNode, viewport image.Rectangle, targets map[int32]accessNode) {
	if sem.Desc.Description == accessHidden {
		return
	}
	bounds := sem.Desc.Bounds.Intersect(viewport)
	if key, err := strconv.ParseInt(sem.Desc.Description, 10, 32); err == nil {
		if node, ok := targets[int32(key)]; ok && !bounds.Empty() {
			node.bounds = bounds
			t.add(int32(key), node)
		}
		return
	}
	if sem.ParentID != 0 && sem.Desc.Label != "" && !bounds.Empty() {
		t.add(-int32(sem.ID), accessNode{name: sem.Desc.Label, role: accessText, bounds: bounds})
	}
	// Gio stores sibling areas in reverse paint order.
	for i := len(sem.Children) - 1; i >= 0; i-- {
		t.append(sem.Children[i], viewport, targets)
	}
}

func (t *accessTree) add(id int32, node accessNode) {
	t.nodes[id] = node
	t.order = append(t.order, id)
	if node.focused && !node.disabled {
		t.focus = id
	}
}
