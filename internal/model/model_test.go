package model

import (
	"database/sql"
	"errors"
	"testing"
)

func TestFolderFilterUsesPathBoundaries(t *testing.T) {
	filter := ParseFilter(`path:C:\Project A`)
	for _, path := range []string{`c:\project a`, `C:\Project A\Cache`} {
		if !filter.Matches(Record{Path: path}, "") {
			t.Fatalf("expected match for %s", path)
		}
	}
	for _, path := range []string{"", `C:\Project AB\Cache`, `D:\Project A\Cache`} {
		if filter.Matches(Record{Path: path}, "") {
			t.Fatalf("unexpected match for %s", path)
		}
	}
	words := ParseFilter("cache LOGS")
	if !words.Matches(Record{Name: "App Cache"}, "old logs") || words.Matches(Record{Name: "App Cache"}, "") {
		t.Fatal("every word should match the name, path, kind or detail")
	}
}

func TestQuickReviewKeyIgnoresDriveSet(t *testing.T) {
	one := []DriveState{{Root: `C:\`, ID: "c"}}
	two := append(one, DriveState{Root: `E:\`, ID: "usb"})
	if ScopeKey(QuickScope, one) != ScopeKey(QuickScope, two) || ScopeKey(QuickScope, nil) != "files-v1|quick review" {
		t.Fatal("adding a fixed drive should keep the Quick review history")
	}
	if ScopeKey(`E:\Backups`, two) == ScopeKey(`E:\Backups`, []DriveState{{Root: `E:\`, ID: "other"}}) {
		t.Fatal("a folder's history should stay tied to its volume")
	}
}

func TestScopeTypeAndSeries(t *testing.T) {
	for scope, want := range map[string]string{"": "quick", "docker": "docker", `C:\`: "drive", "c:": "drive", `C:\Users`: "folder"} {
		if got := ScopeType(scope); got != want {
			t.Errorf("ScopeType(%q) = %q, want %q", scope, got, want)
		}
	}
	if series, err := ChooseSeries(ScopeDocker, "", true); err != nil || series != DockerTotals {
		t.Fatalf("Docker comparisons should default to totals: %q, %v", series, err)
	}
	if series, err := ChooseSeries(ScopeFiles, "large FILES", false); err != nil || series != LargeFiles {
		t.Fatalf("series names should match without case: %q, %v", series, err)
	}
	if _, err := ChooseSeries(ScopeFiles, Images, false); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a Docker series is invalid for files: %v", err)
	}
}

func TestErrorKinds(t *testing.T) {
	err := WithCause(NotFound("no capture for %q", "x"), sql.ErrNoRows)
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || !errors.Is(err, sql.ErrNoRows) ||
		err.Error() != `no capture for "x"` {
		t.Fatalf("unexpected classification: %v", err)
	}
}

func TestRemovalLevels(t *testing.T) {
	for kind, want := range map[string]Removal{
		CacheKind: SafeToClear, RecycleKind: SafeToClear, BuildKind: SafeToClear, TempKind: SafeToClear,
		DownloadKind: CheckFirst, BackupKind: CheckFirst, ScratchKind: CheckFirst, DockerKind: CheckFirst,
		GameKind: Uninstall, ProgramKind: Uninstall,
		SystemKind: Keep, ProjectKind: Keep, PersonalKind: Keep, FilesKind: Keep, "Compose project": Keep,
	} {
		if got := RemovalOf(kind); got != want {
			t.Errorf("RemovalOf(%q) = %q, want %q", kind, got, want)
		}
	}
	if IsCandidate(DockerKind) || IsCandidate(GameKind) || !IsCandidate(DownloadKind) || !IsCandidate(RecoveryKind) {
		t.Fatal("review candidates are the categories to clear or check, apart from Docker storage")
	}
	s := Snapshot{Records: []Record{
		{Series: Categories, Kind: CacheKind, Bytes: 3}, {Series: Categories, Kind: BuildKind, Bytes: 4},
		{Series: Categories, Kind: GameKind, Bytes: 5}, {Series: Folders, Kind: CacheKind, Bytes: 100},
	}}
	totals, ok := RemovalTotals(s)
	if !ok || totals[SafeToClear] != 7 || totals[Uninstall] != 5 || totals[CheckFirst] != 0 {
		t.Fatalf("unexpected removal totals: %v, %t", totals, ok)
	}
	if _, ok := RemovalTotals(Snapshot{}); ok {
		t.Fatal("a capture without category totals has no removal totals")
	}
}
