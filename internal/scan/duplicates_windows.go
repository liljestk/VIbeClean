package scan

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
)

type contentID struct {
	Volume uint64
	File   fileID
}

type contentInfo struct {
	Allocated, Length int64
	Links             uint32
	Deleted, Folder   bool
}

type fingerprint struct {
	id      contentID
	digest  [sha256.Size]byte
	changed int64
	copy    *model.DuplicateCopy
}

type contentReader struct {
	bytes []byte
	path  []uint16
}

var errFileChanged = errors.New("file is unavailable or changed since the scan")

// VerifyDuplicates reads full file contents, one logical size group at a time.
// It skips paths that changed, are busy, or could recall remote data.
func VerifyDuplicates(ctx context.Context, next func() (model.InventoryEntry, error)) (model.DuplicateResult, error) {
	reader := contentReader{bytes: make([]byte, 256*1024), path: make([]uint16, 32768)}
	var result model.DuplicateResult
	var size int64
	files := make(map[contentID]fingerprint)
	byHash := make(map[[sha256.Size]byte][]*model.DuplicateCopy)
	flush := func() {
		for _, copies := range byHash {
			if len(copies) < 2 {
				continue
			}
			group := model.DuplicateGroup{Length: size, Copies: make([]model.DuplicateCopy, 0, len(copies))}
			for _, copy := range copies {
				group.Copies = append(group.Copies, *copy)
			}
			result.Groups = append(result.Groups, group)
		}
		clear(files)
		clear(byHash)
	}
	for {
		if err := ctx.Err(); err != nil {
			return model.DuplicateResult{}, err
		}
		item, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return model.DuplicateResult{}, err
		}
		if item.Length != size {
			flush()
			size = item.Length
		}
		verified, err := reader.verify(ctx, item, files)
		if err != nil {
			result.Skipped++
			continue
		}
		result.Checked++
		if earlier, ok := files[verified.id]; ok && earlier.changed == verified.changed {
			earlier.copy.Paths = append(earlier.copy.Paths, item.Path)
			continue
		}
		files[verified.id] = verified
		byHash[verified.digest] = append(byHash[verified.digest], verified.copy)
	}
	flush()
	slices.SortFunc(result.Groups, func(a, b model.DuplicateGroup) int {
		if order := cmp.Compare(b.Reclaimable(b.SuggestedKeep()), a.Reclaimable(a.SuggestedKeep())); order != 0 {
			return order
		}
		if order := cmp.Compare(b.Length, a.Length); order != 0 {
			return order
		}
		return strings.Compare(a.Copies[0].Paths[0], b.Copies[0].Paths[0])
	})
	return result, nil
}

func (r contentReader) verify(ctx context.Context, item model.InventoryEntry, known map[contentID]fingerprint) (fingerprint, error) {
	path, err := windows.UTF16PtrFromString(longPath(item.Path))
	if err != nil {
		return fingerprint{}, err
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|fileFlagOpenNoRecall|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return fingerprint{}, err
	}
	file := os.NewFile(uintptr(handle), item.Path)
	defer file.Close()
	var basic windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &basic); err != nil {
		return fingerprint{}, err
	}
	changed := basic.LastWriteTime.Nanoseconds()
	if basic.FileAttributes&(attrsDirectory|attrsOffline|attrsReparse) != 0 || changed/int64(time.Millisecond) != item.Modified {
		return fingerprint{}, errFileChanged
	}
	n, err := windows.GetFinalPathNameByHandle(handle, &r.path[0], uint32(len(r.path)), 0)
	if err != nil || n == 0 || n >= uint32(len(r.path)) {
		return fingerprint{}, errFileChanged
	}
	finalPath := windows.UTF16ToString(r.path[:n])
	if !strings.EqualFold(finalPath, longPath(item.Path)) {
		// A saved path can contain an 8.3 alias. Expand the spelling and
		// still require it to match the path of the opened handle.
		n, err := windows.GetLongPathName(path, &r.path[0], uint32(len(r.path)))
		if err != nil || n == 0 || n >= uint32(len(r.path)) || !strings.EqualFold(finalPath, windows.UTF16ToString(r.path[:n])) {
			return fingerprint{}, errFileChanged
		}
	}
	var info contentInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fingerprint{}, err
	}
	if info.Length != item.Length || info.Deleted || info.Folder {
		return fingerprint{}, errFileChanged
	}
	var id contentID
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		id = contentID{Volume: uint64(basic.VolumeSerialNumber), File: fileID{Low: uint64(basic.FileIndexHigh)<<32 | uint64(basic.FileIndexLow)}}
	}
	if id.File == (fileID{}) {
		return fingerprint{}, errFileChanged
	}
	if earlier, ok := known[id]; ok {
		if earlier.changed != changed {
			return fingerprint{}, errFileChanged
		}
		return earlier, nil
	}
	hash := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return fingerprint{}, err
		}
		n, err := file.Read(r.bytes)
		total += int64(n)
		_, _ = hash.Write(r.bytes[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fingerprint{}, err
		}
	}
	if total != item.Length {
		return fingerprint{}, errFileChanged
	}
	return fingerprint{id: id, changed: changed, digest: [sha256.Size]byte(hash.Sum(nil)),
		copy: &model.DuplicateCopy{Paths: []string{item.Path}, Bytes: max(0, info.Allocated), Links: info.Links}}, nil
}
