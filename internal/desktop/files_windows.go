package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/units"
)

type fileViewState struct {
	saved model.Snapshot
	mode  string
	query string
	row   model.RowKey
}

// filesTab holds the Files tab: the latest capture of one location.
type filesTab struct {
	filePane         rankedPane
	files            model.Snapshot
	back             []fileViewState
	fileScope        *walk.ComboBox
	fileMode         *walk.ComboBox
	fileFilter       *walk.LineEdit
	fileStatus       *walk.Label
	scanButton       *walk.PushButton
	browse           *walk.PushButton
	backButton       *walk.PushButton
	settingFileScope bool
	fileLoad         *guiRead
}

func (u *gui) filesPage() d.TabPage {
	return d.TabPage{Title: "Files", Layout: d.VBox{}, Children: []d.Widget{
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Location"},
			d.ComboBox{AssignTo: &u.fileScope, Model: []string{model.QuickScope}, CurrentIndex: 0,
				MinSize: d.Size{Width: 205}, Accessibility: d.Accessibility{Name: "File location"},
				OnCurrentIndexChanged: u.selectFileScope},
			d.PushButton{AssignTo: &u.browse, Text: "Choose folder…", OnClicked: u.chooseFolder},
			d.PushButton{AssignTo: &u.backButton, Text: "Back", Enabled: false, OnClicked: u.goBack},
			d.HSpacer{},
			d.PushButton{AssignTo: &u.scanButton, Text: "Scan now", ToolTipText: "Scan this location (F5)", OnClicked: u.scanClick},
		}},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.Label{Text: "Show"},
			d.ComboBox{AssignTo: &u.fileMode, Model: model.SupportedSeries(model.ScopeFiles, false), CurrentIndex: 0,
				MinSize: d.Size{Width: 165}, OnCurrentIndexChanged: u.renderFiles},
			d.Label{Text: "Search"},
			d.LineEdit{AssignTo: &u.fileFilter, MinSize: d.Size{Width: 175}, OnTextChanged: func() { u.filePane.search(u.fileFilter, "files") },
				Accessibility: d.Accessibility{Name: "Search files"}},
			d.HSpacer{},
		}},
		u.filePane.layout("File usage", "Largest items", [4]string{"Location", "On disk", "Category", "Last modified"},
			"Open in Explorer", "Inspect", false, func() { u.openSelection(&u.filePane) }, u.inspectFolder),
		d.Label{AssignTo: &u.fileStatus, Text: "Ready", NoPrefix: true, EllipsisMode: d.EllipsisEnd},
		d.Label{Text: "Quick review captures daily while selected and the app is open. File allocation is counted once per scan."},
	}}
}

// setFileScope lists Quick review, the fixed drives and saved folders, and selects scope.
func (u *gui) setFileScope(scope string) {
	u.settingFileScope = true
	defer func() { u.settingFileScope = false }()
	options := append([]string{model.QuickScope}, u.drives...)
	for _, saved := range u.scopes {
		if saved.Coverage.ScopeKind == model.ScopeFiles && model.VolumeRoot(saved.Label) != "" && !slices.Contains(options, saved.Label) {
			options = append(options, saved.Label)
		}
	}
	if !slices.Contains(options, scope) {
		options = append(options, scope)
	}
	if current, _ := u.fileScope.Model().([]string); !slices.Equal(current, options) {
		_ = u.fileScope.SetModel(options)
	}
	if selected := slices.Index(options, scope); selected != u.fileScope.CurrentIndex() {
		_ = u.fileScope.SetCurrentIndex(selected)
	}
}

func (u *gui) selectFileScope() {
	if u.settingFileScope || u.fileStatus == nil {
		return
	}
	scope := combo(u.fileScope, model.QuickScope)
	if strings.EqualFold(scope, u.files.Label) {
		return
	}
	u.loadFileScope(scope)
}

func (u *gui) loadFileScope(scope string) {
	backend := u.backend
	if backend == nil {
		return
	}
	u.files = model.Snapshot{Label: scope}
	readForGUI(u, &u.fileLoad, "gui.file_location", func(ctx context.Context) (model.Snapshot, error) {
		return latestSnapshot(ctx, backend, scope)
	}, func(saved model.Snapshot, err error) {
		if err == nil {
			u.files = saved
		}
		u.renderFiles()
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			_ = u.fileStatus.SetText("Could not load the saved scan. " + err.Error())
		}
	})
	u.renderFiles()
}

func (u *gui) scanClick() {
	scope := combo(u.fileScope, model.QuickScope)
	kind := ""
	if strings.EqualFold(scope, u.files.Label) {
		kind = u.files.Coverage.Kind
	}
	u.startFiles(scope, kind, false)
}

// startFiles captures a location. remember keeps the current view for Back.
func (u *gui) startFiles(scope, kind string, remember bool) {
	backend := u.backend
	if backend == nil || u.active != nil {
		return
	}
	if scope != model.QuickScope {
		scope = filepath.Clean(scope)
	}
	if remember && u.files.Label != "" && !strings.EqualFold(scope, u.files.Label) {
		u.rememberView()
	}
	u.setFileScope(scope)
	if !strings.EqualFold(scope, u.files.Label) {
		u.loadFileScope(scope)
	}
	if remember {
		_ = u.fileMode.SetCurrentIndex(0)
		_ = u.fileFilter.SetText("")
	}
	ctx := u.beginCapture(scope, "Scanning "+scope+"…")
	task := u.active
	u.renderFiles()
	go func() {
		defer diag.RecoverPanic(ctx, "gui.files_worker")
		saved, err := backend.Capture(ctx, service.CaptureRequest{Scope: scope, Kind: kind}, func(p service.Progress) {
			u.dispatch(func() {
				if u.active == task && ctx.Err() == nil {
					u.setActivity(fmt.Sprintf("Scanning %s  ·  %d files checked", scope, p.FilesChecked))
				}
			})
		})
		if u.ctx.Err() != nil {
			return
		}
		var disks []model.DriveState
		if err == nil {
			disks, _ = backend.Drives(u.ctx)
		}
		u.applyCapture(task, err, func() {
			if err != nil {
				u.renderFiles()
				return
			}
			cancelGUIRead(&u.fileLoad)
			u.files = saved
			u.setFileScope(saved.Label)
			u.showDrives(disks)
			u.renderFiles()
			u.updateScopes()
		})
	}()
}

func (u *gui) rememberView() {
	state := fileViewState{saved: u.files, mode: combo(u.fileMode, model.Folders), query: u.fileFilter.Text()}
	if row, ok := u.filePane.selected(); ok {
		state.row = model.RowKey{Series: row.Item.Series, Key: row.Item.Key}
	}
	u.back = append(u.back, state)
	if len(u.back) > historyDepth {
		u.back = u.back[len(u.back)-historyDepth:]
	}
}

func (u *gui) chooseFolder() {
	if u.backend == nil || u.active != nil {
		return
	}
	dialog := walk.FileDialog{Title: "Choose a folder on a local fixed drive"}
	if accepted, err := dialog.ShowBrowseFolder(u.window); err != nil {
		diag.Log(u.ctx, slog.LevelWarn, "gui.folder_dialog_failed", slog.String("error_kind", diag.ErrorKind(err)))
		u.setActivity("Could not open the folder chooser. " + err.Error())
	} else if accepted {
		u.startFiles(dialog.FilePath, "", true)
	}
}

func (u *gui) inspectFolder() {
	if row, ok := u.filePane.selected(); ok && row.Item.Directory {
		u.startFiles(row.Item.Path, row.Item.Kind, true)
	}
}

func (u *gui) goBack() {
	if len(u.back) == 0 || u.fileCaptureActive() {
		return
	}
	cancelGUIRead(&u.fileLoad)
	state := u.back[len(u.back)-1]
	u.files = state.saved
	u.back = u.back[:len(u.back)-1]
	u.setFileScope(u.files.Label)
	_ = u.fileMode.SetCurrentIndex(max(0, slices.Index(model.SupportedSeries(model.ScopeFiles, false), state.mode)))
	_ = u.fileFilter.SetText(state.query)
	u.renderFiles()
	u.filePane.restoreSelection(state.row)
	u.updateActions()
}

func (u *gui) renderFiles() {
	if u.fileFilter == nil || u.fileStatus == nil {
		return
	}
	started := time.Now()
	defer func() { logRender(u.ctx, "files", started, len(u.files.Records), len(u.filePane.model.rows)) }()
	series := combo(u.fileMode, model.Folders)
	var rows []displayRow
	for _, item := range u.files.Records {
		if item.Series != series {
			continue
		}
		detail := item.Path + "\r\n\r\n" + units.Size(item.Bytes) + " on disk"
		if item.Directory {
			detail += fmt.Sprintf("  ·  %d files", item.Files)
		}
		detail += "\r\n" + model.KindNote(item.Kind)
		rows = append(rows, displayRow{Item: item, Rank: item.Bytes, Before: -1, After: -1, Detail: detail,
			Cells: [4]string{item.Name, units.Size(item.Bytes), item.Kind, fileDate(item.Modified)}})
	}
	u.filePane.show(rows, u.fileFilter.Text(), u.filesEmptyText())
	if u.files.ID == 0 {
		_ = u.fileStatus.SetText(" ")
		return
	}
	if series == model.Subfolders && !u.files.Coverage.Subfolders {
		_ = u.fileStatus.SetText("Scan this scope to index its subfolders.")
		return
	}
	var total int64
	for _, item := range u.files.Records {
		if item.Series == model.Folders {
			total += item.Bytes
		}
	}
	c := u.files.Coverage
	_ = u.fileStatus.SetText(fmt.Sprintf("Saved %s  ·  %s  ·  %s measured  ·  %d files  ·  %d unreadable  ·  %d repeated links",
		timeText(u.files.Captured), u.files.Label, units.Size(total), c.Checked, len(c.Gaps), c.Shared))
}

func (u *gui) filesEmptyText() string {
	switch {
	case u.files.ID != 0:
		return "No items in this view."
	case u.fileCaptureActive():
		return "Measuring this location…"
	case u.fileLoad != nil:
		return "Loading saved scan…"
	}
	return "Scan this location to see what uses space."
}
