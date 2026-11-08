package main

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// attachParentProcess is ATTACH_PARENT_PROCESS for AttachConsole.
const attachParentProcess = ^uintptr(0)

var (
	attachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	console       = sync.OnceValue(openConsole)
)

// consoleOutput returns where status text goes. The executable is built for
// the Windows GUI subsystem, so it has no standard output unless its parent
// redirected one. Otherwise it attaches to the parent's console, if any.
func consoleOutput() io.Writer { return lazyConsole{} }

// lazyConsole attaches only when something is written, so the desktop app
// never attaches to a console it does not use.
type lazyConsole struct{}

func (lazyConsole) Write(p []byte) (int, error) { return console().Write(p) }

func openConsole() io.Writer {
	if handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && handle != 0 && handle != windows.InvalidHandle {
		return os.Stdout
	}
	if ok, _, _ := attachConsole.Call(attachParentProcess); ok == 0 {
		return io.Discard
	}
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return io.Discard
	}
	return out
}
