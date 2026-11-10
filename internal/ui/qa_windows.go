//go:build qa

package ui

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

// QA builds read SPACE_REVIEW_QA, for example
// "view=files;theme=light;row=2;assistant=1", and put the window in that
// state once the saved history has loaded, so layouts can be checked
// without driving the mouse.
func init() {
	settings := map[string]string{}
	for _, part := range strings.Split(os.Getenv("SPACE_REVIEW_QA"), ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			settings[k] = v
		}
	}
	if len(settings) == 0 {
		return
	}
	done := map[string]bool{}
	once := func(name string, ready bool, apply func()) {
		if _, wanted := settings[name]; wanted && !done[name] && ready {
			done[name] = true
			apply()
		}
	}
	qaFrame = func(u *ui, gtx layout.Context) {
		once("theme", true, func() {
			u.themeFixed = true
			u.th.palette = darkPalette
			switch settings["theme"] {
			case "light":
				u.th.palette = lightPalette
			case "contrast":
				u.th.palette = contrastPalette()
			}
			tintFrame(u.hwnd, u.th.palette)
		})
		once("window", true, func() {
			switch settings["window"] {
			case "narrow":
				u.win.Option(app.Size(640, 540))
			case "min":
				u.win.Option(app.Size(560, 460))
			}
		})
		loaded := u.backend != nil && !u.opening
		once("view", loaded, func() {
			for i, name := range viewNames {
				if strings.EqualFold(name, settings["view"]) {
					u.setView(i)
				}
			}
		})
		once("scope", loaded && u.view == viewFiles && u.active == nil, func() {
			scope := settings["scope"]
			u.setFileScope(scope)
			u.loadFileScope(scope, scope)
		})
		pane := u.activePane()
		ready := loaded && done["view"] == (settings["view"] != "") && pane != nil && len(pane.rows) > 0
		once("inspector", ready, func() { u.inspectorHidden = settings["inspector"] == "off" })
		once("mode", ready, func() {
			box := map[int]*selectBox{viewChanges: &u.changeMode, viewFiles: &u.fileMode, viewDocker: &u.dockerMode}[u.view]
			if n, err := strconv.Atoi(settings["mode"]); err == nil && box != nil {
				box.choose(n)
			}
		})
		once("removal", ready, func() {
			if n, err := strconv.Atoi(settings["removal"]); err == nil && u.view == viewFiles {
				u.fileRemoval.choose(n)
			}
		})
		once("search", ready, func() {
			if search := u.activeSearch(); search != nil {
				search.setText(settings["search"])
				pane.search(settings["search"], "qa")
			}
		})
		once("row", ready, func() {
			if n, err := strconv.Atoi(settings["row"]); err == nil {
				pane.setCurrent(n)
			}
		})
		once("focus", ready, func() { gtx.Execute(key.FocusCmd{Tag: pane}) })
		once("map", ready && u.canShowMap(), func() {
			if u.wideResults || settings["filters"] == "" {
				u.toggleDetails(true)
			}
		})
		once("filters", ready && u.view == viewFiles, u.openFileFilters)
		once("dropdown", ready, func() {
			switch settings["dropdown"] {
			case "direction":
				u.direction.open = true
			case "minimum":
				u.minimum.open = true
			case "mode":
				u.changeMode.open, u.fileMode.open, u.dockerMode.open = u.view == viewChanges, u.view == viewFiles, u.view == viewDocker
			case "removal":
				if u.view == viewFiles {
					u.openFileFilters()
					u.fileFilters.removal.open = true
				}
			case "more":
				u.more.open = true
			}
		})
		once("inspect", ready, func() {
			if pane.onInspect != nil && pane.canInspect() {
				pane.onInspect()
			} else if pane.canOpen() && pane.projectAction {
				pane.onOpen()
			}
		})
		once("busy", loaded, func() {
			ctx, cancel := context.WithCancel(u.ctx)
			u.active = &captureTask{scope: model.QuickScope, ctx: ctx, cancel: cancel, started: time.Now(),
				progress: service.Progress{FilesChecked: 1284311, Saving: settings["busy"] == "saving"}}
			u.setActivity("Scanning Quick review…", toneBusy)
		})
		once("assistant", ready || (loaded && u.view == viewMCP), func() { u.askAssistant() })
		once("export", ready, func() { u.openExport() })
		once("schedule", ready, func() { u.openSchedule() })
		once("duplicates", ready && u.files.Coverage.Inventory, func() { u.openDuplicates() })
		once("coverage", ready && u.view == viewFiles, func() { u.showScanCoverage() })
		once("alert", loaded, func() { u.showAlert("Folder unavailable", `C:\Example\Video exports is no longer available.`) })
		if len(done) < len(settings) {
			gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(100 * time.Millisecond)})
		}
	}
}
