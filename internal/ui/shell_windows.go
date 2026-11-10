package ui

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/unit"
	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const swShowNormal = 1

func unitDp(v float32) unit.Dp { return unit.Dp(v) }

// copyText puts text on the clipboard at the next frame.
func (u *ui) copyText(text string) {
	u.clipboard = &text
	u.win.Invalidate()
}

// flushClipboard runs on the UI goroutine with a frame context.
func (u *ui) flushClipboard(gtx layout.Context) {
	if text := u.clipboard; text != nil {
		u.clipboard = nil
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(*text))})
	}
}

func driveName(root string) string {
	if volume := filepath.VolumeName(root); volume != "" {
		return volume
	}
	return root
}

func driveCaption(disk model.DriveState) string {
	if disk.Total <= 0 {
		return "Free space unavailable"
	}
	return units.Size(disk.Free) + " free of " + units.Size(disk.Total)
}

func (u *ui) openSelection(pane *rankedPane) {
	row, ok := pane.selected()
	if !ok || row.Item.Path == "" {
		return
	}
	path := row.Item.Path
	if !row.Item.Directory {
		u.showFileInExplorer(path)
		return
	}
	u.openFolder(path, "gui.open_selection", "Folder unavailable", true)
}

func (u *ui) showFileInExplorer(path string) {
	go func() {
		if err := revealFile(path); err != nil {
			u.post(func() { u.showAlert("Could not show file in Explorer", err.Error()) })
		}
	}()
}

var (
	shell32                    = windows.NewLazySystemDLL("shell32.dll")
	shParseDisplayName         = shell32.NewProc("SHParseDisplayName")
	shOpenFolderAndSelectItems = shell32.NewProc("SHOpenFolderAndSelectItems")
)

func revealFile(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return err
	}
	defer windows.CoUninitialize()
	var item unsafe.Pointer
	if result, _, _ := shParseDisplayName.Call(uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&item)), 0, 0); result != 0 {
		return syscall.Errno(result)
	}
	defer windows.CoTaskMemFree(item)
	if result, _, _ := shOpenFolderAndSelectItems.Call(uintptr(item), 0, 0, 0); result != 0 {
		return syscall.Errno(result)
	}
	return nil
}

func cleanupShortcut(kind string) (label, target string) {
	switch kind {
	case model.ProgramKind:
		return "Installed apps", "ms-settings:appsfeatures"
	case model.TempKind, model.RecoveryKind, model.SystemKind:
		return "Windows Storage", "ms-settings:storagesense"
	case model.RecycleKind:
		return "Open Recycle Bin", "shell:RecycleBinFolder"
	case model.DockerKind:
		return "Review Docker", ""
	}
	return "", ""
}

func (u *ui) openCleanup(kind string) {
	label, target := cleanupShortcut(kind)
	switch {
	case label == "":
		return
	case kind == model.DockerKind:
		u.setView(viewDocker)
	default:
		go func() {
			if err := openShellURL(target); err != nil {
				u.post(func() { u.showAlert("Could not open "+label, err.Error()) })
			}
		}()
	}
}

// closestFolder returns the nearest existing folder at or above path.
func closestFolder(path string) (string, error) {
	for {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			return path, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("%s is no longer available", path)
		}
		path = parent
	}
}

func exploreFolder(path string) error {
	folder, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// S_FALSE means COM was already initialized on this thread; it still needs uninitializing.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return err
	}
	defer windows.CoUninitialize()
	verb, _ := windows.UTF16PtrFromString("explore")
	return windows.ShellExecute(0, verb, folder, nil, nil, swShowNormal)
}

func (u *ui) openFolder(path, event, title string, findParent bool) {
	ctx := diag.Operation(u.ctx)
	go func() {
		defer diag.RecoverPanic(ctx, event)
		started := time.Now()
		var err error
		if findParent {
			path, err = closestFolder(path)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = exploreFolder(path)
		}
		diag.Duration(ctx, event, started, err)
		if err != nil {
			u.post(func() { u.showAlert(title, err.Error()) })
		}
	}()
}

func openShellURL(target string) error {
	url, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, url, nil, nil, swShowNormal)
}

// The common item dialogs, called through their COM vtables.
var (
	coCreateInstance   = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")
	clsidFileOpen      = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog  = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	clsidFileSave      = windows.GUID{Data1: 0xC0B4E2F3, Data2: 0xBA21, Data3: 0x4773, Data4: [8]byte{0x8D, 0xBA, 0x33, 0x5E, 0xC9, 0x46, 0xEB, 0x8B}}
	iidFileSaveDialog  = windows.GUID{Data1: 0x84BCCD23, Data2: 0x5FDE, Data3: 0x4CDB, Data4: [8]byte{0xAE, 0xA4, 0xAF, 0x64, 0xB8, 0x3D, 0x78, 0xAB}}
	errDialogCancelled = syscall.Errno(0x800704C7)
)

type comObject struct{ vtbl *[32]uintptr }

//go:uintptrescapes
func (o *comObject) call(method int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

func (o *comObject) release() { o.call(2) }

type filterSpec struct{ name, spec *uint16 }

const (
	dlgShow, dlgSetFileTypes, dlgSetOptions, dlgGetOptions = 3, 4, 9, 10
	dlgSetFileName, dlgSetTitle, dlgGetResult              = 15, 17, 20
	dlgSetDefaultExtension                                 = 22
	itemGetDisplayName                                     = 5
	fosPickFolders, fosForceFileSystem, fosOverwritePrompt = 0x20, 0x40, 0x2
	sigdnFileSysPath                                       = 0x80058000
)

// itemDialog shows a common item dialog on a COM thread and returns the
// chosen path, or "" when the user cancels.
func itemDialog(save bool, owner uintptr, configure func(*comObject)) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return "", err
	}
	defer windows.CoUninitialize()
	clsid, iid := &clsidFileOpen, &iidFileOpenDialog
	if save {
		clsid, iid = &clsidFileSave, &iidFileSaveDialog
	}
	var dialog *comObject
	const inprocServer = 1
	if hr, _, _ := coCreateInstance.Call(uintptr(unsafe.Pointer(clsid)), 0, inprocServer,
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&dialog))); hr != 0 || dialog == nil {
		return "", syscall.Errno(hr)
	}
	defer dialog.release()
	configure(dialog)
	if hr := dialog.call(dlgShow, owner); hr != 0 {
		if syscall.Errno(hr) == errDialogCancelled {
			return "", nil
		}
		return "", syscall.Errno(hr)
	}
	var item *comObject
	if hr := dialog.call(dlgGetResult, uintptr(unsafe.Pointer(&item))); hr != 0 || item == nil {
		return "", syscall.Errno(hr)
	}
	defer item.release()
	var name *uint16
	if hr := item.call(itemGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&name))); hr != 0 || name == nil {
		return "", syscall.Errno(hr)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(name))
	return windows.UTF16PtrToString(name), nil
}

func setOptions(dialog *comObject, add, remove uint32) {
	var options uint32
	dialog.call(dlgGetOptions, uintptr(unsafe.Pointer(&options)))
	dialog.call(dlgSetOptions, uintptr(options|add&^remove))
}

func pickFolder(owner uintptr, title string) (string, error) {
	titleText, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return "", err
	}
	path, err := itemDialog(false, owner, func(dialog *comObject) {
		setOptions(dialog, fosPickFolders|fosForceFileSystem, 0)
		dialog.call(dlgSetTitle, uintptr(unsafe.Pointer(titleText)))
	})
	runtime.KeepAlive(titleText)
	return path, err
}

// saveFileDialog asks for a new file. Existing files are refused when the
// file is written, so the dialog does not offer to overwrite them.
func saveFileDialog(owner uintptr, title, name, filterName, pattern, extension string) (string, error) {
	values := make([]*uint16, 5)
	for i, s := range []string{title, name, filterName, pattern, extension} {
		p, err := windows.UTF16PtrFromString(s)
		if err != nil {
			return "", err
		}
		values[i] = p
	}
	filters := []filterSpec{{values[2], values[3]}}
	path, err := itemDialog(true, owner, func(dialog *comObject) {
		setOptions(dialog, fosForceFileSystem, fosOverwritePrompt)
		dialog.call(dlgSetTitle, uintptr(unsafe.Pointer(values[0])))
		dialog.call(dlgSetFileTypes, 1, uintptr(unsafe.Pointer(&filters[0])))
		dialog.call(dlgSetFileName, uintptr(unsafe.Pointer(values[1])))
		dialog.call(dlgSetDefaultExtension, uintptr(unsafe.Pointer(values[4])))
	})
	runtime.KeepAlive(values)
	runtime.KeepAlive(filters)
	return path, err
}

const bundleExecutable = "server/space-review.exe"

// writeClaudeBundle saves the executable and its launch arguments as a Claude
// Desktop extension. The client opens the same history when it starts the server.
func writeClaudeBundle(path, executable string, args []string) error {
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	return writeNewFile(path, func(dst io.Writer) error { return packClaudeBundle(dst, source, args) })
}

// writeNewFile publishes a complete file without replacing an existing one.
func writeNewFile(path string, write func(io.Writer) error) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".space-review-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writeErr := write(file)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	from, err := windows.UTF16PtrFromString(file.Name())
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// Another process may create the destination while the save dialog is open.
	return windows.MoveFile(from, to)
}

func packClaudeBundle(dst io.Writer, executable io.Reader, args []string) error {
	bundle := zip.NewWriter(dst)
	manifest, err := bundle.Create("manifest.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(manifest).Encode(map[string]any{
		"manifest_version": "0.3",
		"name":             "space-review",
		"display_name":     "Space Review",
		"version":          strings.TrimPrefix(buildinfo.Version, "v"),
		"description":      "Find large files, review disk usage, and compare saved storage scans on Windows.",
		"author":           map[string]string{"name": "Space Review"},
		"compatibility":    map[string][]string{"platforms": {"win32"}},
		"tools_generated":  true,
		"server": map[string]any{
			"type":        "binary",
			"entry_point": bundleExecutable,
			"mcp_config":  map[string]any{"command": "${__dirname}/" + bundleExecutable, "args": args},
		},
	}); err != nil {
		return err
	}
	entry, err := bundle.Create(bundleExecutable)
	if err != nil {
		return err
	}
	if _, err := io.Copy(entry, executable); err != nil {
		return err
	}
	return bundle.Close()
}
