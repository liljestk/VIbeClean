package ui

import (
	"context"
	"strings"
	"time"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
)

type recycleDialog struct {
	item      model.Record
	identity  recycleIdentity
	preparing *guiRead
	moving    bool
	focused   bool
	scrim     bool
	move      widget.Clickable
	cancel    widget.Clickable
}

func (u *ui) canOfferDelete(item model.Record) bool {
	return u.files.ID != 0 && !strings.EqualFold(item.Path, u.files.Label) && recyclable(item)
}

func (u *ui) openRecycle(gtx layout.Context, item model.Record) {
	if u.view != viewFiles || u.active != nil || u.recycle != nil || u.alert != nil || !u.canOfferDelete(item) {
		return
	}
	if u.details != nil {
		u.closeDetails(gtx)
	} else {
		u.returnFocus = &u.deleteButton
	}
	d := &recycleDialog{item: item}
	u.recycle = d
	readForGUI(u, &d.preparing, "gui.recycle_check", func(context.Context) (recycleIdentity, error) {
		return recycleIdentityAt(item)
	}, func(identity recycleIdentity, err error) {
		if err != nil {
			u.closeRecycle()
			u.showAlert("Could not move item to the Recycle Bin", err.Error())
			return
		}
		d.identity = identity
	})
	gtx.Execute(op.InvalidateCmd{})
}

func (u *ui) closeRecycle() {
	if d := u.recycle; d != nil && !d.moving {
		cancelGUIRead(&d.preparing)
		u.recycle = nil
	}
}

func (d *recycleDialog) confirm(u *ui) {
	d.moving = true
	ctx, owner := diag.Operation(u.ctx), u.hwnd
	go func() {
		defer diag.RecoverPanic(ctx, "gui.recycle")
		started := time.Now()
		err := recycleItem(ctx, owner, d.item, d.identity)
		diag.Duration(ctx, "gui.recycle", started, err)
		u.post(func() {
			d.moving = false
			u.closeRecycle()
			if err != nil {
				u.showAlert("Could not move item to the Recycle Bin", err.Error())
				return
			}
			u.returnFocus = &u.inspectorButton
			parent := u.fileParent
			if d.item.Directory && parent != "" {
				folder, removed := model.NormalizedKey(parent), model.NormalizedKey(d.item.Path)
				if folder == removed || strings.HasPrefix(folder, removed+`\`) {
					parent = ""
				}
			}
			saved := u.files
			u.startFileScan(saved.Label, saved.Coverage.Kind, false, saved.Coverage.Exclusions, true, parent)
			u.setActivity("Moved to Recycle Bin. Scanning "+saved.Label+"…", toneBusy)
		})
	}()
}

func (d *recycleDialog) layout(gtx layout.Context, u *ui) {
	if d.cancel.Clicked(gtx) && !d.moving {
		u.closeRecycle()
		gtx.Execute(op.InvalidateCmd{})
		return
	}
	if d.move.Clicked(gtx) && !d.moving && d.preparing == nil {
		d.confirm(u)
	}
	if !d.focused {
		gtx.Execute(key.FocusCmd{Tag: &d.cancel})
		d.focused = true
	}
	title := "Move file to Recycle Bin?"
	note := "Items in the Recycle Bin still use disk space."
	if d.item.Directory {
		title = "Move folder to Recycle Bin?"
		note = "This moves the entire folder and its contents. " + note
	}
	if d.preparing != nil {
		note = "Checking the selected item…"
	}
	if d.moving {
		title, note = "Moving to Recycle Bin…", "Space Review will rescan this location when the move finishes."
	}
	t := u.th
	modal(gtx, t, &d.scrim, 560, 0, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(17, t.ink), title) }),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				style := t.body(t.ink).wrapped(-1)
				style.wrap = text.WrapGraphemes
				return t.label(gtx, style, d.item.Path)
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.small(t.quiet).wrapped(3), note) }),
			layout.Rigid(space(0, 16)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flow{gap: 8, end: true}.Layout(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.cancel, buttonStyle{label: "Cancel", disabled: d.moving})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.move, buttonStyle{label: "Move to Recycle Bin", kind: primary, disabled: d.moving || d.preparing != nil})
					})
			}),
		)
	})
}
