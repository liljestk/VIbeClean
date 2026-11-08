package main

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
)

func TestWindowsAllocationAndHardLinks(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "backup"), filepath.Join(root, "other")
	for _, path := range []string{a, b} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	payload := filepath.Join(a, "data.bin")
	if err := os.WriteFile(payload, make([]byte, mib), 0600); err != nil {
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
	if err := windows.DeviceIoControl(windows.Handle(f.Fd()), 0x900c4, nil, 0, nil, 0, &written, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, 2<<30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s, err := captureFiles(context.Background(), root, "", nil)
	if err != nil || s.Coverage.Checked != 3 || s.Coverage.Shared != 1 || s.Coverage.Estimated != 0 {
		t.Fatalf("capture coverage: %+v, %v", s.Coverage, err)
	}
	var allocated int64
	for _, item := range recordsIn(s, folders) {
		allocated += item.Bytes
	}
	if allocated <= mib || allocated >= 2*mib || len(recordsIn(s, largeFiles)) != 0 {
		t.Fatalf("unexpected sparse allocation: %d, records: %+v", allocated, s.Records)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := captureFiles(ctx, root, "", nil); !errors.Is(err, context.Canceled) {
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
	s, err := captureFiles(context.Background(), root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored int64
	for _, item := range recordsIn(s, folders) {
		stored += item.Bytes
	}
	if stored <= 0 || stored >= int64(len(payload)) || s.Coverage.Links != 0 {
		t.Fatalf("compressed file should count its stored bytes: %d of %d, coverage %+v", stored, len(payload), s.Coverage)
	}
}

func TestQuickReviewKeyIgnoresDriveSet(t *testing.T) {
	one := []driveState{{Root: `C:\`, ID: "c"}}
	two := append(one, driveState{Root: `E:\`, ID: "usb"})
	if scopeKey(quickScope, one) != scopeKey(quickScope, two) || savedScopeKey(quickScope) != scopeKey(quickScope, nil) {
		t.Fatal("adding a fixed drive should keep the Quick review history")
	}
	if scopeKey(`E:\Backups`, two) == scopeKey(`E:\Backups`, []driveState{{Root: `E:\`, ID: "other"}}) {
		t.Fatal("a folder's history should stay tied to its volume")
	}
	if !drivesReplaced(two, []driveState{{Root: `C:\`, ID: "c"}, {Root: `E:\`, ID: "other"}}) || drivesReplaced(two, one) {
		t.Fatal("only a different volume at the same root counts as a replaced drive")
	}
}

func TestJournalCacheUsesFreshnessAndIdentity(t *testing.T) {
	s := snapshot{ID: 1, Captured: time.Now().Add(-time.Hour).UnixMilli(), Coverage: coverage{
		Subfolders: true, Drives: []driveState{{ID: "drive", Journal: 1, NextUSN: 90, Free: 100}},
	}}
	disks := append([]driveState(nil), s.Coverage.Drives...)
	if refreshAtStartup(s, disks) {
		t.Fatal("matching recent drive and journal should reuse the cached display")
	}
	disks[0].NextUSN++
	if !refreshAtStartup(s, disks) {
		t.Fatal("journal activity should trigger a refresh")
	}
}

func TestDailyCaptureDueUsesElapsedTime(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	s := snapshot{ID: 1, Captured: now.Add(-24 * time.Hour).UnixMilli()}
	if !dailyCaptureDue(s, now) || dailyCaptureDue(s, now.Add(-time.Millisecond)) ||
		dailyCaptureDue(snapshot{}, now) {
		t.Fatal("daily capture should become due at 24 hours after a saved capture")
	}
}
