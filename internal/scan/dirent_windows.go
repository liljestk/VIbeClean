package scan

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	attrsDirectory = uint32(windows.FILE_ATTRIBUTE_DIRECTORY)
	attrsReparse   = uint32(windows.FILE_ATTRIBUTE_REPARSE_POINT)
	// Offline, recall-on-open and recall-on-data-access mark cloud files whose
	// data is not on this disk.
	attrsOffline = uint32(0x441000)
	// residentLimit is the longest file data an MFT record can hold.
	residentLimit = int64(4096)
	// cloudTagMask keeps the cloud placeholder tag and drops its subtype bits.
	cloudTagMask = 0xffff0fff
	cloudTag     = 0x9000001a

	fileIDExtdDirectoryInfo = 19
	fileFlagOpenNoRecall    = 0x00100000
	shareAll                = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	// dirInfoHeader is the size of FILE_ID_EXTD_DIR_INFO before the UTF-16 name.
	dirInfoHeader = 88
)

var (
	kernel32     = windows.NewLazySystemDLL("kernel32.dll")
	extendedList = kernel32.NewProc("GetFileInformationByHandleEx")
	storedSize   = kernel32.NewProc("GetCompressedFileSizeW")
)

type fileID struct {
	Low, High uint64
}

type dirEntry struct {
	Name     string
	ID       fileID
	Bytes    int64
	Length   int64
	Modified int64
	Attrs    uint32
	Tag      uint32
}

func (e dirEntry) directory() bool { return e.Attrs&attrsDirectory != 0 }

// link reports a reparse point other than a cloud placeholder: a junction,
// mount point or symbolic link that leads somewhere else.
func (e dirEntry) link() bool {
	return e.Attrs&attrsReparse != 0 && e.Tag&cloudTagMask != cloudTag
}

// directoryReader lists folders in metadata batches and counts its work for
// diagnostics. It reuses one buffer, so a callback must not start another listing.
type directoryReader struct {
	buffer       [64 * 1024]byte
	estimated    int64
	directories  int64
	batches      int64
	sizeLookups  int64
	sizeFailures int64
	gapKinds     map[string]int
}

func longPath(path string) string { return `\\?\` + path }

// compressedSize returns the bytes a file stores on disk, including data kept
// by the Windows Overlay Filter. It reads metadata only and returns 0 on failure.
func (r *directoryReader) compressedSize(path string) int64 {
	r.sizeLookups++
	ptr, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		r.sizeFailures++
		return 0
	}
	var high uint32
	low, _, callErr := storedSize.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xffffffff && callErr != windows.ERROR_SUCCESS {
		r.sizeFailures++
		return 0
	}
	return max(0, int64(high)<<32|int64(uint32(low)))
}

// entries lists a folder without opening file contents or following a final
// link. Names of files smaller than namedFrom are left empty to save work,
// apart from the names that can identify a folder.
func (r *directoryReader) entries(ctx context.Context, path string, namedFrom int64, consume func(dirEntry)) error {
	r.directories++
	ptr, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(ptr, windows.FILE_LIST_DIRECTORY, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT|fileFlagOpenNoRecall, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buf := r.buffer[:]
	for started := false; ; started = true {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.batches++
		ok, _, queryErr := extendedList.Call(uintptr(handle), fileIDExtdDirectoryInfo, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if ok != 0 {
			if err := r.parseBatch(buf, path, namedFrom, consume); err != nil {
				return err
			}
			continue
		}
		if errors.Is(queryErr, windows.ERROR_NO_MORE_FILES) || errors.Is(queryErr, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		if !started && unsupported(queryErr) {
			return r.fallback(ctx, path, consume)
		}
		return queryErr
	}
}

func unsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

// parseBatch decodes one buffer of FILE_ID_EXTD_DIR_INFO records.
func (r *directoryReader) parseBatch(buf []byte, path string, namedFrom int64, consume func(dirEntry)) error {
	for offset := 0; ; {
		if offset < 0 || offset+dirInfoHeader > len(buf) {
			return errInvalidOffset
		}
		record := buf[offset:]
		step := int(binary.LittleEndian.Uint32(record))
		nameLength := int(binary.LittleEndian.Uint32(record[60:]))
		if nameLength%2 != 0 || nameLength > len(record)-dirInfoHeader {
			return errInvalidName
		}
		entry := r.decode(record, nameLength, path, namedFrom)
		if entry.Name != "." && entry.Name != ".." {
			consume(entry)
		}
		if step == 0 {
			return nil
		}
		if step < dirInfoHeader+nameLength || step%8 != 0 {
			return errInvalidLength
		}
		offset += step
	}
}

func (r *directoryReader) decode(record []byte, nameLength int, path string, namedFrom int64) dirEntry {
	entry := dirEntry{
		Attrs:    binary.LittleEndian.Uint32(record[56:]),
		Bytes:    max(0, int64(binary.LittleEndian.Uint64(record[48:]))),
		Length:   int64(binary.LittleEndian.Uint64(record[40:])),
		Modified: filetimeMillis(binary.LittleEndian.Uint64(record[24:])),
		Tag:      binary.LittleEndian.Uint32(record[68:]),
		ID:       fileID{binary.LittleEndian.Uint64(record[72:]), binary.LittleEndian.Uint64(record[80:])},
	}
	if nameLength == 0 {
		return entry
	}
	name := func() string {
		return windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&record[dirInfoHeader])), nameLength/2))
	}
	// Directory listings report zero allocation for WOF-compressed files
	// (CompactOS, compact /exe) because the data lives in an alternate stream.
	// Files this long cannot be resident in the MFT, so ask for their real size.
	if entry.Bytes == 0 && entry.Length > residentLimit && entry.Attrs&(attrsDirectory|attrsOffline) == 0 {
		entry.Name = name()
		entry.Bytes = r.compressedSize(filepath.Join(path, entry.Name))
	}
	if entry.Name == "" && (entry.directory() || entry.Bytes >= namedFrom ||
		markerName(record[dirInfoHeader:dirInfoHeader+nameLength])) {
		entry.Name = name()
	}
	return entry
}

// markerName reports whether a UTF-16 file name is one that can identify its
// folder, such as CACHEDIR.TAG or go.mod. It does not allocate.
func markerName(raw []byte) bool {
	var lower [32]byte
	n := len(raw) / 2
	if n > len(lower) {
		return false
	}
	for i := range n {
		c := binary.LittleEndian.Uint16(raw[2*i:])
		if c >= 0x80 {
			return false
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = byte(c)
	}
	return markerFile(string(lower[:n]))
}

func filetimeMillis(ticks uint64) int64 {
	const unixEpoch = 116444736000000000
	if ticks <= unixEpoch {
		return 0
	}
	return int64((ticks - unixEpoch) / 10000)
}

// fallback lists folders on file systems without extended directory records.
// It reports logical sizes, so the capture marks these folders as estimated.
func (r *directoryReader) fallback(ctx context.Context, path string, consume func(dirEntry)) error {
	r.estimated++
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, ok, err := fallbackEntry(path, entry)
		if err != nil {
			return err
		}
		if ok {
			consume(item)
		}
	}
	return nil
}

func fallbackEntry(path string, entry os.DirEntry) (dirEntry, bool, error) {
	info, err := entry.Info()
	if os.IsNotExist(err) {
		return dirEntry{}, false, nil
	}
	if err != nil {
		return dirEntry{}, false, err
	}
	ptr, err := windows.UTF16PtrFromString(longPath(filepath.Join(path, entry.Name())))
	if err != nil {
		return dirEntry{}, false, err
	}
	attrs, err := windows.GetFileAttributes(ptr)
	if os.IsNotExist(err) {
		return dirEntry{}, false, nil
	}
	if err != nil {
		return dirEntry{}, false, err
	}
	size := info.Size()
	if attrs&attrsOffline != 0 {
		size = 0
	}
	return dirEntry{Name: entry.Name(), Bytes: size, Length: info.Size(), Attrs: attrs, Modified: info.ModTime().UnixMilli()}, true, nil
}
