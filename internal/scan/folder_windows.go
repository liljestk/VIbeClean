package scan

import (
	"context"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/volume"
)

// LocalFolder checks that a path is a folder on a local fixed drive, reached
// without junctions or links, and returns its cleaned form.
func LocalFolder(path string) (string, error) {
	if len(path) == 2 && path[1] == ':' {
		path += `\`
	}
	if !filepath.IsAbs(path) || model.VolumeRoot(path) == "" {
		return "", model.InvalidArgument("choose an absolute folder on a local fixed drive")
	}
	path = filepath.Clean(path)
	root := model.VolumeRoot(path)
	if !volume.IsFixed(root) {
		return "", model.InvalidArgument("choose a folder on a local fixed drive")
	}
	for current := path; !strings.EqualFold(current, root); current = filepath.Dir(current) {
		ptr, err := windows.UTF16PtrFromString(longPath(current))
		if err != nil {
			return "", err
		}
		var data windows.Win32finddata
		handle, err := windows.FindFirstFile(ptr, &data)
		if err != nil {
			return "", err
		}
		_ = windows.FindClose(handle)
		e := dirEntry{Attrs: data.FileAttributes, Tag: data.Reserved0}
		if !e.directory() || e.link() {
			return "", model.InvalidArgument("choose the folder's original location instead of a junction or link")
		}
	}
	return path, nil
}

// FileAllocation returns the bytes a file stores on disk. It reads the
// directory record of the file's folder, so it never opens the file. found is
// false when the folder has no regular file of that name.
func FileAllocation(ctx context.Context, path string) (size int64, found bool, err error) {
	reader := new(directoryReader)
	name := filepath.Base(path)
	err = reader.entries(ctx, filepath.Dir(path), 0, func(entry dirEntry) {
		if !entry.directory() && !entry.link() && strings.EqualFold(entry.Name, name) {
			size, found = entry.Bytes, true
		}
	})
	return size, found, err
}
