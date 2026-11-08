package scan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const fsctlSetSparse = 0x900c4

func capture(t *testing.T, ctx context.Context, root string) (model.Snapshot, error) {
	t.Helper()
	return Capture(ctx, Request{Scope: root})
}

func TestWindowsAllocationAndHardLinks(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "backup"), filepath.Join(root, "other")
	for _, path := range []string{a, b} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	payload := filepath.Join(a, "data.bin")
	if err := os.WriteFile(payload, make([]byte, units.MiB), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(payload, filepath.Join(b, "alias.bin")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(b, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var written uint32
	if err := windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &written, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, 2<<30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s, err := capture(t, context.Background(), root)
	if err != nil || s.Coverage.Checked != 3 || s.Coverage.Shared != 1 || s.Coverage.Estimated != 0 {
		t.Fatalf("capture coverage: %+v, %v", s.Coverage, err)
	}
	var allocated int64
	for _, item := range model.RecordsIn(s, model.Folders) {
		allocated += item.Bytes
	}
	if allocated <= units.MiB || allocated >= 2*units.MiB || len(model.RecordsIn(s, model.LargeFiles)) != 0 {
		t.Fatalf("unexpected sparse allocation: %d, records: %+v", allocated, s.Records)
	}
	if candidates := model.RecordsIn(s, model.Candidates); len(candidates) != 1 || candidates[0].Kind != model.BackupKind {
		t.Fatalf("the backup folder should be a review candidate: %+v", candidates)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := capture(t, ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan returned %v", err)
	}
}

func TestCompressedFilesCountStoredBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compressed.txt")
	payload := bytes.Repeat([]byte("space review compressible payload\r\n"), 1<<16)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	// compact.exe applies the Windows Overlay Filter compression that CompactOS uses.
	// Directory listings then report zero allocation for the file.
	if output, err := exec.Command("compact.exe", "/c", "/exe:xpress4k", path).CombinedOutput(); err != nil {
		t.Skipf("WOF compression is unavailable here: %v %s", err, output)
	}
	s, err := capture(t, context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var stored int64
	for _, item := range model.RecordsIn(s, model.Folders) {
		stored += item.Bytes
	}
	if stored <= 0 || stored >= int64(len(payload)) || s.Coverage.Links != 0 {
		t.Fatalf("compressed file should count its stored bytes: %d of %d, coverage %+v", stored, len(payload), s.Coverage)
	}
	size, found, err := FileAllocation(context.Background(), path)
	if err != nil || !found || size != stored {
		t.Fatalf("FileAllocation = %d, %t, %v; want %d", size, found, err, stored)
	}
}

func TestLocalFolderRejectsInvalidLocations(t *testing.T) {
	for _, path := range []string{"relative", `\\server\share`} {
		if _, err := LocalFolder(path); !errors.Is(err, model.ErrInvalidArgument) {
			t.Errorf("LocalFolder(%q) = %v, want an invalid argument", path, err)
		}
	}
	dir := t.TempDir()
	if got, err := LocalFolder(dir + `\`); err != nil || got != filepath.Clean(dir) {
		t.Fatalf("LocalFolder(%q) = %q, %v", dir, got, err)
	}
}

func TestRefreshRecommendedUsesFreshnessAndIdentity(t *testing.T) {
	s := model.Snapshot{ID: 1, Captured: time.Now().Add(-time.Hour).UnixMilli(), Coverage: model.Coverage{
		Subfolders: true, Drives: []model.DriveState{{ID: "drive", Journal: 1, NextUSN: 90, Free: 100}},
	}}
	disks := append([]model.DriveState(nil), s.Coverage.Drives...)
	if RefreshRecommended(s, disks) {
		t.Fatal("matching recent drive and journal should reuse the saved capture")
	}
	disks[0].NextUSN++
	if !RefreshRecommended(s, disks) {
		t.Fatal("journal activity should recommend a refresh")
	}
	if !RefreshRecommended(model.Snapshot{}, disks) {
		t.Fatal("a missing capture should recommend a refresh")
	}
}

func TestDrivesReplaced(t *testing.T) {
	one := []model.DriveState{{Root: `C:\`, ID: "c"}}
	two := append(one, model.DriveState{Root: `E:\`, ID: "usb"})
	if !drivesReplaced(two, []model.DriveState{{Root: `C:\`, ID: "c"}, {Root: `E:\`, ID: "other"}}) || drivesReplaced(two, one) {
		t.Fatal("only a different volume at the same root counts as a replaced drive")
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, parent string
		directory    bool
		root         bool
		want         string
	}{
		{"Windows.old", model.FilesKind, true, true, model.RecoveryKind},
		{"Program Files", model.FilesKind, true, true, model.SystemKind},
		{"Program Files", model.FilesKind, true, false, model.FilesKind},
		{"node_modules", model.ProjectKind, true, false, model.BuildKind},
		{"node_modules", model.AppKind, true, false, model.AppKind},
		{"site-backup.zip", model.FilesKind, false, false, model.BackupKind},
		{"photos.zip", model.FilesKind, false, false, model.FilesKind},
		{"crash.dmp", model.FilesKind, false, false, model.CacheKind},
		{"feature-wt", model.FilesKind, true, false, model.CheckoutKind},
		{"anything", model.SystemKind, true, false, model.SystemKind},
	} {
		if got := classify(tc.name, tc.parent, tc.directory, tc.root); got != tc.want {
			t.Errorf("classify(%q, %q) = %q, want %q", tc.name, tc.parent, got, tc.want)
		}
	}
}
