package ui

import (
	"context"
	"strconv"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/schedule"
)

type scheduleDialog struct {
	scope         string
	manager       schedule.Manager
	state         schedule.State
	request       *guiRead
	problem       string
	at, threshold searchField
	save, remove  widget.Clickable
	close         widget.Clickable
	scrim         bool
}

func (u *ui) openSchedule() {
	backend := u.backend
	if backend == nil || u.opts.MCPCommand == "" {
		return
	}
	var scope string
	switch u.view {
	case viewFiles:
		scope = u.fileScopeValue()
	case viewChanges:
		scope = u.changeSelection().Label
	case viewDocker:
		scope = model.DockerScope
	default:
		return
	}
	executable := u.opts.MCPCommand
	d := &scheduleDialog{scope: scope}
	d.at = searchField{plain: true, prompt: "Time (HH:MM)"}
	d.threshold = searchField{plain: true, prompt: "Free space (GiB)"}
	d.at.setText("09:00")
	d.threshold.setText("0")
	u.scheduled = d
	type loadedSchedule struct {
		manager schedule.Manager
		state   schedule.State
	}
	readForGUI(u, &d.request, "gui.schedule_read", func(ctx context.Context) (loadedSchedule, error) {
		info, err := backend.Info(ctx)
		if err != nil {
			return loadedSchedule{}, err
		}
		if info.IndexPath == "" {
			return loadedSchedule{}, model.Unavailable("The backend did not report its index path.")
		}
		manager := schedule.Manager{Index: info.IndexPath, Executable: executable}
		state, err := manager.Read(ctx, scope)
		return loadedSchedule{manager, state}, err
	}, func(data loadedSchedule, err error) {
		d.manager = data.manager
		d.applyState(data.state, err)
	})
}

func (d *scheduleDialog) applyState(state schedule.State, err error) {
	if err != nil {
		d.problem = err.Error()
		return
	}
	d.state, d.problem = state, state.Problem
	d.at.setText(state.Config.At)
	d.threshold.setText(strconv.FormatInt(state.Config.ThresholdGiB, 10))
}

func (d *scheduleDialog) change(u *ui, remove bool) {
	if d.request != nil || d.manager.Index == "" {
		return
	}
	manager := d.manager
	var config schedule.Config
	if !remove {
		threshold, err := strconv.ParseInt(strings.TrimSpace(d.threshold.text()), 10, 64)
		if err != nil {
			d.problem = "Enter a whole number of GiB. Use 0 to turn alerts off."
			return
		}
		config = schedule.Config{Scope: d.scope, At: strings.TrimSpace(d.at.text()), ThresholdGiB: threshold}
		if err := config.Validate(); err != nil {
			d.problem = err.Error()
			return
		}
	}
	readForGUI(u, &d.request, "gui.schedule_write", func(ctx context.Context) (schedule.State, error) {
		var err error
		if remove {
			err = manager.Remove(ctx, d.scope)
		} else {
			err = manager.Save(ctx, config)
		}
		if err != nil {
			return schedule.State{}, err
		}
		return manager.Read(ctx, d.scope)
	}, d.applyState)
}

func (u *ui) closeSchedule() {
	if u.scheduled != nil {
		cancelGUIRead(&u.scheduled.request)
		u.scheduled = nil
	}
}

func (d *scheduleDialog) scheduleStatus() string {
	if d.request != nil {
		return "Checking Windows Task Scheduler…"
	}
	status := "Background capture is off for this location."
	if d.state.Registered {
		status = "The Windows task is disabled."
		if d.state.Enabled {
			status = "Next scheduled time " + timeText(d.state.Config.Next(time.Now()).UnixMilli())
		}
	}
	outcome := d.state.Outcome
	if outcome.Attempted != 0 {
		status += "\nLast run " + timeText(outcome.Attempted) + " · " + outcome.Result
	}
	if outcome.LastSaved != 0 && outcome.Result != "Scan saved." {
		status += "\nLast saved scan " + timeText(outcome.LastSaved)
	}
	if outcome.Alert != "" {
		status += "\nLow space · " + outcome.Alert
	}
	if outcome.AlertProblem != "" {
		status += "\n" + outcome.AlertProblem
	}
	return status
}

func (d *scheduleDialog) layout(gtx layout.Context, u *ui) {
	if d.close.Clicked(gtx) {
		u.closeSchedule()
		return
	}
	if d.save.Clicked(gtx) {
		d.change(u, false)
	}
	if d.remove.Clicked(gtx) {
		d.change(u, true)
	}
	t := u.th
	disabled := d.request != nil || d.manager.Index == ""
	modal(gtx, t, &d.scrim, 570, 0, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(17, t.ink), "Schedule scans")
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.strong(13, t.ink).wrapped(2), d.scope)
			}),
			layout.Rigid(space(0, 4)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.quiet).wrapped(2),
					"Runs daily while you’re signed in, idle and plugged in, even with Space Review closed.")
			}),
			layout.Rigid(space(0, 12)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return labeled(gtx, t, "Local time (HH:MM)", func(gtx layout.Context) layout.Dimensions {
					dims, _ := d.at.Layout(gtx, t, disabled)
					return dims
				})
			}),
			layout.Rigid(space(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return labeled(gtx, t, "Alert at or below (GiB)", func(gtx layout.Context) layout.Dimensions {
					dims, _ := d.threshold.Layout(gtx, t, disabled)
					return dims
				})
			}),
			layout.Rigid(space(0, 4)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.quiet), "Enter 0 to turn off alerts.")
			}),
			layout.Rigid(space(0, 12)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.ink).wrapped(6), d.scheduleStatus())
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.small(t.grow).wrapped(3), d.problem)
			}),
			layout.Rigid(space(0, 12)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flow{gap: 8, rowGap: 8, end: true}.Layout(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.remove, buttonStyle{kind: quiet, label: "Turn off", disabled: disabled || !d.state.Registered})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.close, buttonStyle{kind: quiet, label: "Close"})
					},
					func(gtx layout.Context) layout.Dimensions {
						return t.button(gtx, &d.save, buttonStyle{kind: primary, label: "Save schedule", disabled: disabled})
					},
				)
			}),
		)
	})
}
