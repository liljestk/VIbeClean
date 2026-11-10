package ui

import (
	"context"
	"errors"
	"slices"
	"time"

	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

const projectItems = "Project items"

// cardKeys are the Docker totals shown in the summary, in order.
var cardKeys = []string{"host", model.Images, model.Containers, model.Volumes, model.BuildCache}

// dockerTab holds the latest Docker engine capture.
type dockerTab struct {
	dockerPane    rankedPane
	docker        model.Snapshot
	dockerMode    selectBox
	dockerFilter  searchField
	idleOnly      checkBox
	dockerBack    widget.Clickable
	dockerOpened  bool
	dockerProject string
	dockerCards   []metric
	dockerNotes   string
}

func (u *ui) initDocker() {
	u.dockerPane.init([4]column{{title: "Project"}, {title: "Size", end: true}, {title: "Type", drop: 2}, {title: "Activity", drop: 1}},
		false, "Largest items")
	u.dockerPane.ctx = u.ctx
	u.dockerPane.projectAction = true
	u.dockerPane.openText = "Show project items"
	u.dockerPane.onOpen = u.inspectProject
	u.dockerMode.setOptions(model.SupportedSeries(model.ScopeDocker, false), 0)
	u.dockerFilter.prompt, u.dockerFilter.shortcut = "Search Docker resources", findShortcut
}

func (u *ui) startDocker() {
	backend := u.backend
	if backend == nil || u.active != nil {
		return
	}
	u.dockerOpened = true
	ctx := u.beginCapture(model.DockerScope, "Measuring Docker layers and volumes. This can take a minute.")
	task := u.active
	u.renderDocker()
	go func() {
		defer diag.RecoverPanic(ctx, "gui.docker_worker")
		saved, err := backend.Capture(ctx, service.CaptureRequest{Scope: model.DockerScope}, u.captureProgress(task))
		if u.ctx.Err() != nil {
			return
		}
		u.applyCapture(task, err, func() {
			if err != nil {
				u.renderDocker()
				if !errors.Is(err, context.Canceled) {
					u.setActivity(err.Error(), toneProblem)
				}
				return
			}
			u.docker = saved
			u.renderDocker()
			u.updateScopes()
		})
	}()
}

func (u *ui) dockerMetrics() []metric {
	if u.docker.ID == 0 {
		return nil
	}
	names := []string{"Windows disk", model.Images, model.Containers, model.Volumes, model.BuildCache}
	cards := make([]metric, len(names))
	for i, name := range names {
		cards[i] = metric{label: name, value: "—"}
	}
	for _, item := range u.docker.Records {
		if item.Series != model.DockerTotals {
			continue
		}
		slot := slices.Index(cardKeys, item.Key)
		if slot < 0 {
			continue
		}
		caption := item.Activity
		if slot == 0 {
			caption = "Allocated virtual disk"
		} else if caption == "" {
			caption = "Engine usage"
		}
		cards[slot].value, cards[slot].caption = units.Size(item.Bytes), caption
	}
	return cards
}

func (u *ui) renderDocker() {
	started := time.Now()
	defer func() { logRender(u.ctx, "docker", started, len(u.docker.Records), len(u.dockerPane.rows)) }()
	u.dockerCards = u.dockerMetrics()
	mode := u.dockerMode.value()
	if mode == "" {
		mode = model.Projects
	}
	u.dockerPane.columns[0].title = seriesNoun(mode)
	idle := u.idleApplies()
	var rows []displayRow
	for _, item := range u.docker.Records {
		if !u.showsDockerItem(mode, item) || (idle && u.idleOnly.value.Value && !item.Idle) {
			continue
		}
		var facts []fact
		if item.Kind != "" {
			facts = append(facts, fact{"Type", item.Kind})
		}
		if item.Activity != "" {
			facts = append(facts, fact{"Activity", item.Activity})
		}
		if item.Reclaimable > 0 {
			facts = append(facts, fact{"Reclaimable", units.Size(item.Reclaimable)})
		}
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: -1, After: -1, Detail: item.Detail, Facts: facts,
			Cells: [4]string{item.Name, units.Size(item.Bytes), item.Kind, item.Activity}})
	}
	empty := "No resources in this view."
	if u.docker.ID == 0 {
		empty = "Refresh Docker to see what uses space."
		if u.active != nil && u.active.scope == model.DockerScope {
			empty = "Waiting for Docker usage…"
		}
	}
	u.dockerPane.show(rows, u.dockerFilter.text(), empty)
	u.dockerNotes = ""
	if u.docker.ID != 0 {
		u.dockerNotes = u.docker.Coverage.Engine + "  ·  Projects show writable layers and volumes. The Windows disk has its own total."
	}
}

func (u *ui) idleApplies() bool {
	mode := u.dockerMode.value()
	return mode != model.Projects && mode != model.DockerTotals && mode != projectItems
}

func (u *ui) showsDockerItem(mode string, item model.Record) bool {
	if mode == projectItems {
		return (item.Series == model.Volumes || item.Series == model.Containers) && item.Group == u.dockerProject
	}
	return item.Series == mode
}

func (u *ui) inspectProject() {
	if row, ok := u.dockerPane.selected(); ok && row.Item.Series == model.Projects {
		u.dockerProject = row.Item.Group
		options := append(model.SupportedSeries(model.ScopeDocker, false), projectItems)
		u.dockerMode.setOptions(options, len(options)-1)
		u.dockerFilter.setText("")
		u.idleOnly.value.Value = false
		u.renderDocker()
	}
}

func (u *ui) backDocker() {
	u.dockerMode.setOptions(model.SupportedSeries(model.ScopeDocker, false), 0)
	u.renderDocker()
	for i, source := range u.dockerPane.rows {
		if u.dockerPane.items[source].Item.Group == u.dockerProject {
			u.dockerPane.setCurrent(i)
			break
		}
	}
}

func (u *ui) dockerListTitle() string {
	switch mode := u.dockerMode.value(); mode {
	case projectItems:
		return "Items in " + u.dockerProject
	case model.Projects, "":
		return "Docker projects"
	default:
		return mode
	}
}

func (u *ui) dockerView(gtx layout.Context) layout.Dimensions {
	t := u.th
	if u.dockerMode.Changed() {
		if u.dockerMode.value() != projectItems && len(u.dockerMode.options) > len(model.SupportedSeries(model.ScopeDocker, false)) {
			index := u.dockerMode.index
			u.dockerMode.setOptions(model.SupportedSeries(model.ScopeDocker, false), index)
		}
		u.renderDocker()
	}
	if u.dockerBack.Clicked(gtx) {
		u.backDocker()
	}
	var children []layout.FlexChild
	if len(u.dockerCards) > 0 {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.summaryCard(gtx, u.dockerCards) }),
			layout.Rigid(space(0, 16)))
	}
	children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
		var back *widget.Clickable
		if u.dockerMode.value() == projectItems {
			back = &u.dockerBack
		}
		return u.results(gtx, &u.dockerPane, resultsHeader{
			title:    u.dockerListTitle(),
			count:    itemCount(len(u.dockerPane.rows), u.docker.ID != 0),
			notes:    u.dockerNotes,
			back:     back,
			backText: "Back to projects",
			search: func(gtx layout.Context) layout.Dimensions {
				d, changed := u.dockerFilter.Layout(gtx, t, u.docker.ID == 0)
				if changed {
					u.dockerPane.search(u.dockerFilter.text(), "docker")
				}
				return d
			},
			filters: []layout.Widget{
				func(gtx layout.Context) layout.Dimensions { return u.dockerMode.Layout(gtx, t) },
				func(gtx layout.Context) layout.Dimensions {
					d, changed := u.idleOnly.Layout(gtx, t, "Idle only", !u.idleApplies())
					if changed {
						u.renderDocker()
					}
					return d
				},
			},
		})
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}
