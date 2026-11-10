package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
)

func recyclable(item model.Record) bool {
	switch item.Series {
	case model.Folders, model.Subfolders, model.LargeFiles, model.Candidates, model.Labeled:
	default:
		return false
	}
	switch item.Kind {
	case model.SystemKind, model.ProgramKind, model.GameKind, model.RecycleKind,
		model.RecoveryKind, model.DockerKind, model.VirtualKind:
		return false
	}
	path, root := item.Path, model.VolumeRoot(item.Path)
	if root == "" || len(path) <= len(root) || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		!filepath.IsLocal(path[len(root):]) || strings.ContainsAny(path, "*?<>|\x00\"") {
		return false
	}
	for part := range strings.SplitSeq(path[len(root):], `\`) {
		if strings.TrimRight(part, ". ") != part {
			return false
		}
	}
	return true
}

type recycleIdentity struct {
	volume, high, low uint32
	created, modified windows.Filetime
	length            uint64
}

var (
	errRecycleChanged = errors.New("This item has changed. Scan the location again before deleting it.")
	errRecycleDenied  = errors.New("Windows could not recycle the selected item.")
)

// Read the exact entry without opening its contents. A recorded file may have
// changed since the scan, and a path may now point through a junction.
func recycleIdentityAt(item model.Record) (recycleIdentity, error) {
	if !recyclable(item) {
		return recycleIdentity{}, errRecycleDenied
	}
	root := model.VolumeRoot(item.Path)
	drive, _ := windows.UTF16PtrFromString(root)
	if windows.GetDriveType(drive) != windows.DRIVE_FIXED {
		return recycleIdentity{}, errors.New("Choose an item on a local fixed drive.")
	}
	for parent := filepath.Dir(item.Path); !strings.EqualFold(parent, root); parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return recycleIdentity{}, err
		}
		if !info.IsDir() {
			return recycleIdentity{}, errors.New("Choose the item's original location instead of a junction or link.")
		}
	}
	name, err := windows.UTF16PtrFromString(`\\?\` + item.Path)
	if err != nil {
		return recycleIdentity{}, err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return recycleIdentity{}, err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return recycleIdentity{}, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_SYSTEM) != 0 {
		return recycleIdentity{}, errors.New("Open this location in Explorer to review the protected or linked item.")
	}
	directory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	length := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if directory != item.Directory || (!directory &&
		((item.Modified > 0 && item.Modified != info.LastWriteTime.Nanoseconds()/int64(time.Millisecond)) ||
			(item.Logical > 0 && uint64(item.Logical) != length))) {
		return recycleIdentity{}, errRecycleChanged
	}
	return recycleIdentity{
		volume: info.VolumeSerialNumber, high: info.FileIndexHigh, low: info.FileIndexLow,
		created: info.CreationTime, modified: info.LastWriteTime, length: length,
	}, nil
}

var (
	shCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	clsidFileOperation          = iid("{3ad05575-8857-4850-9277-11b85bdb8e09}")
	iidFileOperation            = iid("{947aab5f-0a5c-4c13-b4d6-4bf7836fc9f8}")
	iidShellItem                = iid("{43826d1e-e718-42ee-bc55-a1e261c37bfe}")
	iidFileOperationSink        = iid("{04b0f1a7-9490-44bc-96e1-4296a31252e2}")
)

const (
	fileOpSetFlags = 5
	fileOpSetOwner = 9
	fileOpDelete   = 18
	fileOpPerform  = 21
	fofSilent      = 0x0004
	fofNoConfirm   = 0x0010
	fofNoErrorUI   = 0x0400
	fofNoConnected = 0x2000
	fofxRecycle    = 0x00080000
	fofxEarlyFail  = 0x00100000
	tsfRecycle     = 0x0080
	hresultAbort   = 0x80004004
	hresultPointer = 0x80004003
	hresultNoIface = 0x80004002
	hresultNotImpl = 0x80004001
)

// The x64 Windows calling convention allows the unused callbacks to ignore
// their arguments. This sink handles one item in a synchronous file operation.
var (
	recycleNoop       = syscall.NewCallback(func(uintptr) uintptr { return 0 })
	recycleUnexpected = syscall.NewCallback(func(uintptr) uintptr { return hresultNotImpl })
	recycleSinkTable  = [...]uintptr{
		syscall.NewCallback((*recycleSink).queryInterface),
		syscall.NewCallback((*recycleSink).addRef),
		syscall.NewCallback((*recycleSink).release),
		recycleNoop, recycleNoop, // StartOperations, FinishOperations
		recycleUnexpected, recycleUnexpected, // PreRenameItem, PostRenameItem
		recycleUnexpected, recycleUnexpected, // PreMoveItem, PostMoveItem
		recycleUnexpected, recycleUnexpected, // PreCopyItem, PostCopyItem
		syscall.NewCallback((*recycleSink).preDelete),
		syscall.NewCallback((*recycleSink).postDelete),
		recycleUnexpected, recycleUnexpected, // PreNewItem, PostNewItem
		recycleNoop, recycleNoop, recycleNoop, recycleNoop, // UpdateProgress, ResetTimer, PauseTimer, ResumeTimer
	}
)

type recycleSink struct {
	vtbl     *uintptr
	refs     atomic.Int32
	ctx      context.Context
	item     model.Record
	identity recycleIdentity
	err      error
	moved    bool
}

func (s *recycleSink) queryInterface(id *windows.GUID, out **recycleSink) uintptr {
	if out == nil {
		return hresultPointer
	}
	*out = nil
	if *id != iidUnknown && *id != iidFileOperationSink {
		return hresultNoIface
	}
	*out = s
	s.addRef()
	return 0
}

func (s *recycleSink) addRef() uintptr  { return uintptr(s.refs.Add(1)) }
func (s *recycleSink) release() uintptr { return uintptr(s.refs.Add(-1)) }

func (s *recycleSink) preDelete(flags uintptr, shellItem *comObject) uintptr {
	// The shell can fall back to permanent deletion if recycling is unavailable.
	if flags&tsfRecycle == 0 || shellItem == nil {
		s.err = errRecycleDenied
		return hresultAbort
	}
	s.err = s.verify(shellItem)
	if s.err != nil {
		return hresultAbort
	}
	return 0
}

func (s *recycleSink) verify(shellItem *comObject) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	var path *uint16
	if hr := shellItem.call(itemGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&path))); hr != 0 {
		return syscall.Errno(hr)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(path))
	item := s.item
	item.Path = windows.UTF16PtrToString(path)
	if !strings.EqualFold(item.Path, s.item.Path) {
		return errRecycleChanged
	}
	actual, err := recycleIdentityAt(item)
	if err != nil {
		return err
	}
	if actual != s.identity {
		return errRecycleChanged
	}
	return nil
}

func (s *recycleSink) postDelete(_, _ uintptr, result uintptr, recycled *comObject) uintptr {
	if s.err != nil {
		return 0
	}
	if int32(result) < 0 {
		s.err = syscall.Errno(uint32(result))
	} else {
		s.moved = recycled != nil
	}
	return 0
}

func recycleItem(ctx context.Context, owner uintptr, item model.Record, identity recycleIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(item.Path)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return err
	}
	defer windows.CoUninitialize()

	var shellItem *comObject
	if hr, _, _ := shCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(name)), 0,
		uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&shellItem))); hr != 0 {
		return syscall.Errno(hr)
	}
	defer shellItem.release()

	sink := &recycleSink{vtbl: &recycleSinkTable[0], ctx: ctx, item: item, identity: identity}
	sink.refs.Store(1)
	var pin runtime.Pinner
	pin.Pin(sink)
	defer pin.Unpin()
	var operation *comObject
	const inprocServer = 1
	if hr, _, _ := coCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOperation)), 0, inprocServer,
		uintptr(unsafe.Pointer(&iidFileOperation)), uintptr(unsafe.Pointer(&operation))); hr != 0 {
		return syscall.Errno(hr)
	}
	defer operation.release()
	if hr := operation.call(fileOpSetOwner, owner); hr != 0 {
		return syscall.Errno(hr)
	}
	const flags = fofxRecycle | fofxEarlyFail | fofNoConnected | fofNoErrorUI | fofSilent | fofNoConfirm
	if hr := operation.call(fileOpSetFlags, flags); hr != 0 {
		return syscall.Errno(hr)
	}
	if hr := operation.call(fileOpDelete, uintptr(unsafe.Pointer(shellItem)), uintptr(unsafe.Pointer(sink))); hr != 0 {
		return syscall.Errno(hr)
	}
	result := operation.call(fileOpPerform)
	switch {
	case sink.err != nil:
		return sink.err
	case sink.moved:
		return nil
	case int32(result) < 0:
		return syscall.Errno(uint32(result))
	default:
		return errRecycleDenied
	}
}
