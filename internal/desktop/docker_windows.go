package desktop

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

const projectItems = "Project items"

// cardKeys are the Docker totals shown as cards, in order.
var cardKeys = []string{"host", model.Images, model.Containers, model.Volumes, model.BuildCache}

// dockerTab holds the Docker tab: the latest engine capture.
type dockerTab struct {
	dockerPane     rankedPane
	docker         model.Snapshot
	dockerMode     *walk.ComboBox
	dockerFilter   *walk.LineEdit
	idleOnly       *walk.CheckBox
	dockerStatus   *walk.Label
	dockerButton   *walk.PushButton
	dockerCards    [5]*walk.Label
	dockerCaptions [5]*walk.Label
	dockerOpened   bool
	dockerProject  string
}

func (u *gui) dockerPage() d.TabPage {
	var cards []d.Widget
	for index, name := range []string{"Windows disk", model.Images, model.Containers, model.Volumes, model.BuildCache} {
		cards = append(cards, d.GroupBox{Title: name, StretchFactor: 1,
			MinSize: d.Size{Width: 155},
			Layout:  d.VBox{Margins: d.Margins{Left: 10, Top: 3, Right: 10, Bottom: 5}},
			Children: []d.Widget{
				d.Label{AssignTo: &u.dockerCards[index], Text: "—", Font: d.Font{PointSize: 15, Bold: true}},
				d.Label{AssignTo: &u.dockerCaptions[index], Text: " "},
			}})
	}
	return d.TabPage{Title: "Docker", Layout: d.VBox{}, Children: []d.Widget{
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: cards},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Show"},
			d.ComboBox{AssignTo: &u.dockerMode, Model: model.SupportedSeries(model.ScopeDocker, false), CurrentIndex: 0,
				MinSize: d.Size{Width: 150}, OnCurrentIndexChanged: u.renderDocker},
			d.Label{Text: "Search"},
			d.LineEdit{AssignTo: &u.dockerFilter, MinSize: d.Size{Width: 175}, OnTextChanged: func() { u.dockerPane.search(u.dockerFilter, "docker") },
				Accessibility: d.Accessibility{Name: "Search Docker resources"}},
			d.CheckBox{AssignTo: &u.idleOnly, Text: "Idle only", OnCheckedChanged: u.renderDocker},
			d.HSpacer{}, d.PushButton{AssignTo: &u.dockerButton, Text: "Refresh Docker",
				ToolTipText: "Capture current Docker usage (F5)", OnClicked: u.startDocker},
		}},
		u.dockerPane.layout("Docker resources", "Largest items", [4]string{"Resource", "Size", "Type", "Activity"},
			"Show project items", "", true, u.inspectProject, nil),
		d.Label{AssignTo: &u.dockerStatus, Text: "Refresh Docker to measure the local engine.", NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Label{Text: "Projects show writable layers and volumes. The Windows disk has its own total."},
	}}
}

func (u *gui) startDocker() {
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
		saved, err := backend.Capture(ctx, service.CaptureRequest{Scope: model.DockerScope}, nil)
		if u.ctx.Err() != nil {
			return
		}
		u.applyCapture(task, err, func() {
			if err != nil {
				u.renderDocker()
				if !errors.Is(err, context.Canceled) {
					_ = u.dockerStatus.SetText(err.Error())
				}
				return
			}
			u.docker = saved
			u.renderDocker()
			u.updateScopes()
		})
	}()
}

func (u *gui) renderDockerCards() {
	for _, item := range u.docker.Records {
		if item.Series != model.DockerTotals {
			continue
		}
		slot := slices.Index(cardKeys, item.Key)
		if slot < 0 {
			continue
		}
		_ = u.dockerCards[slot].SetText(units.Size(item.Bytes))
		caption := item.Activity
		if slot == 0 {
			caption = "Allocated virtual disk"
		} else if caption == "" {
			caption = "Engine usage"
		}
		_ = u.dockerCaptions[slot].SetText(caption)
	}
}

func (u *gui) renderDocker() {
	if u.idleOnly == nil || u.dockerFilter == nil || u.dockerStatus == nil {
		return
	}
	started := time.Now()
	defer func() { logRender(u.ctx, "docker", started, len(u.docker.Records), len(u.dockerPane.model.rows)) }()
	u.renderDockerCards()
	mode := combo(u.dockerMode, model.Projects)
	idle := mode != model.Projects && mode != model.DockerTotals
	u.idleOnly.SetEnabled(idle)
	var rows []displayRow
	for _, item := range u.docker.Records {
		if !u.showsDockerItem(mode, item) || (idle && u.idleOnly.Checked() && !item.Idle) {
			continue
		}
		detail := item.Name + "\r\n" + units.Size(item.Bytes) + "\r\n\r\n" + item.Detail
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: -1, After: -1, Detail: detail,
			Cells: [4]string{item.Name, units.Size(item.Bytes), item.Kind, item.Activity}})
	}
	empty := "No resources in this view."
	if u.docker.ID == 0 {
		empty = "Refresh Docker to see what uses space."
		if u.active != nil && u.active.scope == model.DockerScope {
			empty = "Waiting for Docker usage…"
		}
	}
	u.dockerPane.show(rows, u.dockerFilter.Text(), empty)
	if u.docker.ID != 0 {
		_ = u.dockerStatus.SetText(u.docker.Coverage.Engine + "  ·  Saved " + timeText(u.docker.Captured))
	}
}

func (u *gui) showsDockerItem(mode string, item model.Record) bool {
	if mode == projectItems {
		return (item.Series == model.Volumes || item.Series == model.Containers) && item.Group == u.dockerProject
	}
	return item.Series == mode
}

func (u *gui) inspectProject() {
	if row, ok := u.dockerPane.selected(); ok && row.Item.Series == model.Projects {
		u.dockerProject = row.Item.Group
		options := append(model.SupportedSeries(model.ScopeDocker, false), projectItems)
		_ = u.dockerMode.SetModel(options)
		_ = u.dockerMode.SetCurrentIndex(len(options) - 1)
		_ = u.dockerFilter.SetText("")
		u.idleOnly.SetChecked(false)
		u.renderDocker()
	}
}
