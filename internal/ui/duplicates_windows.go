package ui

import (
	"context"
	"fmt"
	"path/filepath"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

type duplicatePath struct {
	copy int
	path string
}

type duplicatesDialog struct {
	location          string
	result            model.DuplicateResult
	request           *guiRead
	problem           string
	group             int
	kept              map[int]int
	paths             []duplicatePath
	actions           map[int]*widget.Clickable
	list              widget.List
	prev, next, close widget.Clickable
	scrim             bool
}

func (u *ui) openDuplicates() {
	backend, saved, folder := u.backend, u.files, u.fileParent
	if backend == nil || !saved.Coverage.Inventory || u.active != nil {
		return
	}
	location := saved.Label
	if folder != "" {
		location = folder
	}
	d := &duplicatesDialog{location: location, kept: make(map[int]int)}
	u.duplicates = d
	readForGUI(u, &d.request, "gui.duplicates", func(ctx context.Context) (model.DuplicateResult, error) {
		return backend.Duplicates(ctx, saved.ID, folder)
	}, func(result model.DuplicateResult, err error) {
		if err != nil {
			d.problem = err.Error()
			return
		}
		d.result = result
		if len(result.Groups) > 0 {
			d.showGroup(0)
		}
	})
}

func (u *ui) closeDuplicates() {
	if u.duplicates != nil {
		cancelGUIRead(&u.duplicates.request)
		u.duplicates = nil
	}
}

func (d *duplicatesDialog) showGroup(index int) {
	d.group = index
	g := d.result.Groups[index]
	if _, ok := d.kept[index]; !ok {
		d.kept[index] = g.SuggestedKeep()
	}
	d.paths = d.paths[:0]
	for i, copy := range g.Copies {
		d.paths = append(d.paths, duplicatePath{copy: i})
		for _, path := range copy.Paths {
			d.paths = append(d.paths, duplicatePath{copy: i, path: path})
		}
	}
	d.list = widget.List{}
	d.actions = make(map[int]*widget.Clickable)
}

func (d *duplicatesDialog) layout(gtx layout.Context, u *ui) {
	if d.close.Clicked(gtx) {
		u.closeDuplicates()
		return
	}
	if d.prev.Clicked(gtx) && d.group > 0 {
		d.showGroup(d.group - 1)
	}
	if d.next.Clicked(gtx) && d.group+1 < len(d.result.Groups) {
		d.showGroup(d.group + 1)
	}
	t := u.th
	modal(gtx, t, &d.scrim, 760, 480, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return spread(gtx, 10,
					func(gtx layout.Context) layout.Dimensions {
						return t.label(gtx, t.strong(18, t.ink), "Duplicate files")
					},
					func(gtx layout.Context) layout.Dimensions {
						label := "Close"
						if d.request != nil {
							label = "Cancel"
						}
						return t.button(gtx, &d.close, buttonStyle{kind: quiet, label: label})
					})
			}),
			layout.Rigid(space(0, 6)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.quiet).wrapped(2), d.location)
			}),
			layout.Rigid(space(0, 12)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return d.body(gtx, u) }),
		)
	})
}

func (d *duplicatesDialog) body(gtx layout.Context, u *ui) layout.Dimensions {
	t := u.th
	if d.request != nil {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.ink).wrapped(2), "Reading local files of matching sizes to verify their contents…")
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.progress(gtx, t) }),
		)
	}
	if d.problem != "" {
		return t.label(gtx, t.body(t.grow).wrapped(4), d.problem)
	}
	coverage := fmt.Sprintf("%s paths checked · %s online-only · %s unavailable or changed",
		thousands(d.result.Checked), thousands(d.result.Offline), thousands(d.result.Skipped))
	if len(d.result.Groups) == 0 {
		return t.label(gtx, t.body(t.ink).wrapped(3), "No verified duplicates in this scan.\n"+coverage)
	}
	g := d.result.Groups[d.group]
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return flow{gap: 8, rowGap: 5, middle: true}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &d.prev, buttonStyle{icon: iconChevronLeft, label: "Previous set", compact: true, disabled: d.group == 0})
				},
				func(gtx layout.Context) layout.Dimensions {
					return t.label(gtx, t.strong(14, t.ink), fmt.Sprintf("Set %s of %s", thousands(int64(d.group+1)), thousands(int64(len(d.result.Groups)))))
				},
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &d.next, buttonStyle{icon: iconChevronRight, label: "Next set", compact: true, disabled: d.group+1 == len(d.result.Groups)})
				},
			)
		}),
		layout.Rigid(space(0, 8)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.strong(14, t.ink).wrapped(2),
				fmt.Sprintf("%s copies · %s per file · up to %s reclaimable", thousands(int64(len(g.Copies))),
					units.Size(g.Length), units.Size(g.Reclaimable(d.kept[d.group]))))
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.label(gtx, t.small(t.quiet).wrapped(2), coverage)
		}),
		layout.Rigid(space(0, 8)),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			d.list.Axis = layout.Vertical
			first, last := len(d.paths), -1
			list := material.List(&material.Theme{FingerSize: 38}, &d.list)
			list.AnchorStrategy = material.Occupy
			list.Indicator.Color, list.Indicator.HoverColor = withAlpha(t.quiet, 90), withAlpha(t.quiet, 170)
			dims := list.Layout(gtx, len(d.paths), func(gtx layout.Context, index int) layout.Dimensions {
				first, last = min(first, index), max(last, index)
				return d.pathRow(gtx, u, index, g)
			})
			for index := range d.actions {
				if index < first || index > last {
					delete(d.actions, index)
				}
			}
			return dims
		}),
	)
}

func (d *duplicatesDialog) pathRow(gtx layout.Context, u *ui, index int, group model.DuplicateGroup) layout.Dimensions {
	path := d.paths[index]
	action := d.actions[index]
	if action == nil {
		action = new(widget.Clickable)
		d.actions[index] = action
	}
	t := u.th
	if path.path == "" {
		if action.Clicked(gtx) {
			d.kept[d.group] = path.copy
			gtx.Execute(op.InvalidateCmd{})
		}
		kept := d.kept[d.group] == path.copy
		copy := group.Copies[path.copy]
		tag := units.Size(copy.Bytes) + " on disk"
		if len(copy.Paths) > 1 {
			tag += fmt.Sprintf(" · %d hard-linked paths", len(copy.Paths))
		}
		if uint64(copy.Links) > uint64(len(copy.Paths)) {
			tag += " · links outside this set"
		}
		return t.panel(gtx, layout.Inset{Top: 5, Bottom: 5, Left: 10, Right: 6}, 6, t.well,
			func(gtx layout.Context) layout.Dimensions {
				return spread(gtx, 8,
					func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return t.label(gtx, t.strong(13, t.ink), fmt.Sprintf("Copy %d", path.copy+1))
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return t.label(gtx, t.small(t.quiet).wrapped(2), tag)
							}),
						)
					},
					func(gtx layout.Context) layout.Dimensions {
						label := "Keep this copy"
						if kept {
							label = "Keeping"
						}
						gtx.Constraints.Min.X = min(gtx.Constraints.Max.X, gtx.Dp(108))
						return t.button(gtx, action, buttonStyle{label: label, compact: true, pressed: kept,
							accessibleName: fmt.Sprintf("%s · copy %d", label, path.copy+1)})
					})
			})
	}
	if action.Clicked(gtx) {
		u.showFileInExplorer(path.path)
	}
	return layout.Inset{Top: 5, Bottom: 5, Left: 10, Right: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return spread(gtx, 8,
			func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return t.label(gtx, t.strong(14, t.ink), filepath.Base(path.path))
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						style := t.small(t.quiet).wrapped(2)
						style.wrap = text.WrapGraphemes
						return t.label(gtx, style, path.path)
					}),
				)
			},
			func(gtx layout.Context) layout.Dimensions {
				return t.button(gtx, action, buttonStyle{kind: quiet, icon: iconFolder, label: "Show in Explorer", compact: true,
					accessibleName: "Show " + filepath.Base(path.path) + " in Explorer"})
			})
	})
}
