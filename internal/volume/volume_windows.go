// Package volume reads the local fixed drives: free space, volume identity
// and NTFS change-journal position.
package volume

import (
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
)

const (
	driveFixed           = 3
	fsctlQueryUSNJournal = 0x900f4
)

// FixedRoots lists the roots of local fixed drives, such as "C:\".
func FixedRoots() []string {
	bits, _ := windows.GetLogicalDrives()
	var roots []string
	for index := range 26 {
		if bits&(1<<index) != 0 {
			root := fmt.Sprintf("%c:\\", 'A'+index)
			if IsFixed(root) {
				roots = append(roots, root)
			}
		}
	}
	return roots
}

// IsFixed reports whether a root such as "C:\" is a local fixed drive.
func IsFixed(root string) bool {
	ptr, err := windows.UTF16PtrFromString(root)
	return err == nil && windows.GetDriveType(ptr) == driveFixed
}

// States reads each root. Drives that cannot be read are left out.
func States(roots []string) []model.DriveState {
	var states []model.DriveState
	for _, root := range roots {
		if state, ok := read(root); ok {
			states = append(states, state)
		}
	}
	return states
}

func read(root string) (model.DriveState, bool) {
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return model.DriveState{}, false
	}
	var total, free uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, nil, &total, &free); err != nil {
		return model.DriveState{}, false
	}
	var name [64]uint16
	id := strings.ToLower(root)
	if windows.GetVolumeNameForVolumeMountPoint(ptr, &name[0], uint32(len(name))) == nil {
		id = strings.ToLower(windows.UTF16ToString(name[:]))
	}
	state := model.DriveState{Root: root, ID: id, Total: int64(total), Free: int64(free)}
	// This query on a root directory handle works without elevation on NTFS.
	handle, err := windows.CreateFile(ptr, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		var data [80]byte
		var count uint32
		if windows.DeviceIoControl(handle, fsctlQueryUSNJournal, nil, 0, &data[0], uint32(len(data)), &count, nil) == nil && count >= 24 {
			state.Journal = binary.LittleEndian.Uint64(data[:])
			state.NextUSN = int64(binary.LittleEndian.Uint64(data[16:]))
		}
		_ = windows.CloseHandle(handle)
	}
	return state, true
}
