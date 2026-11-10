package schedule

import (
	"context"
	"errors"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	createWindow    = user32.NewProc("CreateWindowExW")
	destroyWindow   = user32.NewProc("DestroyWindow")
	loadIcon        = user32.NewProc("LoadIconW")
	shellNotifyIcon = windows.NewLazySystemDLL("shell32.dll").NewProc("Shell_NotifyIconW")
)

// notifyIconData matches the Unicode NOTIFYICONDATA structure.
type notifyIconData struct {
	Size        uint32
	Window      uintptr
	ID          uint32
	Flags       uint32
	Message     uint32
	Icon        uintptr
	Tip         [128]uint16
	State       uint32
	StateMask   uint32
	Info        [256]uint16
	Version     uint32
	Title       [64]uint16
	InfoFlags   uint32
	GUID        windows.GUID
	BalloonIcon uintptr
}

// Notify sends a local shell notification from an interactive scheduled job.
func Notify(ctx context.Context, title, message string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	class, _ := windows.UTF16PtrFromString("STATIC")
	hwnd, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	if hwnd == 0 {
		return err
	}
	defer destroyWindow.Call(hwnd)
	icon, _, _ := loadIcon.Call(0, 32512) // IDI_APPLICATION
	const (
		addIcon    = 0
		removeIcon = 2
		nifIcon    = 0x2
		nifTip     = 0x4
		nifInfo    = 0x10
		warning    = 0x2
	)
	data := notifyIconData{Window: hwnd, ID: 1, Flags: nifIcon | nifTip | nifInfo, Icon: icon, InfoFlags: warning}
	data.Size = uint32(unsafe.Sizeof(data))
	notificationText(data.Title[:], title)
	notificationText(data.Info[:], message)
	notificationText(data.Tip[:], "Space Review")
	if result, _, _ := shellNotifyIcon.Call(addIcon, uintptr(unsafe.Pointer(&data))); result == 0 {
		return errors.New("Windows could not show the low-space notification")
	}
	defer func() { shellNotifyIcon.Call(removeIcon, uintptr(unsafe.Pointer(&data))) }()
	// Keep the shell icon alive long enough for the notice to be shown.
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func notificationText(dst []uint16, text string) {
	if value, err := windows.UTF16FromString(text); err == nil {
		copy(dst[:len(dst)-1], value)
	}
}
