package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

func TestKindOfPath(t *testing.T) {
	for path, want := range map[string]string{
		`C:\`:                                                             model.FilesKind,
		`C:\Windows\Temp`:                                                 model.SystemKind,
		`C:\Windows.old`:                                                  model.RecoveryKind,
		`C:\$Recycle.Bin\S-1-5-21`:                                        model.RecycleKind,
		`C:\Program Files\Mozilla Firefox`:                                model.ProgramKind,
		`C:\Program Files\Mozilla Firefox\logs`:                           model.ProgramKind,
		`C:\Program Files\WindowsApps`:                                    model.SystemKind,
		`C:\Program Files\Windows Defender`:                               model.SystemKind,
		`C:\Program Files (x86)\Steam`:                                    model.ProgramKind,
		`C:\Program Files (x86)\Steam\appcache`:                           model.CacheKind,
		`C:\Program Files (x86)\Steam\steamapps`:                          model.GameKind,
		`c:\program files (x86)\steam\steamapps\common\Portal 2\bin`:      model.GameKind,
		`D:\SteamLibrary\steamapps\shadercache\620`:                       model.PossibleCleanupKind,
		`D:\SteamLibrary\steamapps\downloading`:                           model.PossibleCleanupKind,
		`C:\Program Files\Epic Games\Fortnite\Cache`:                      model.GameKind,
		`C:\Program Files\Epic Games\Launcher`:                            model.ProgramKind,
		`C:\Program Files (x86)\Ubisoft\Ubisoft Game Launcher\games\Anno`: model.GameKind,
		`C:\XboxGames\Halo Infinite`:                                      model.GameKind,
		`C:\Users\ana\AppData\Local\Temp\build-1`:                         model.TempKind,
		`C:\Users\ana\AppData\Local\Programs\Microsoft VS Code`:           model.ProgramKind,
		`C:\Users\ana\AppData\Roaming\Code\Cache`:                         model.PossibleCleanupKind,
		`C:\Users\ana\AppData\Roaming\Code\User`:                          model.AppKind,
		`C:\Users\ana\AppData\Local\npm-cache\_cacache`:                   model.PackageKind,
		`C:\Users\ana\.nuget\packages\newtonsoft.json`:                    model.PackageKind,
		`C:\Users\ana\go\pkg\mod`:                                         model.PackageKind,
		`C:\Users\ana\.cargo\registry`:                                    model.PackageKind,
		`C:\Users\ana\.cache\huggingface`:                                 model.PossibleCleanupKind,
		`C:\Users\ana\.vscode\extensions\ext\node_modules`:                model.AppKind,
		`C:\Users\ana\Downloads\setup`:                                    model.DownloadKind,
		`C:\Users\ana\Documents\Notes`:                                    model.PersonalKind,
		`C:\Users\ana\OneDrive - Contoso\Pictures`:                        model.PersonalKind,
		`C:\Users\ana\Documents\Backups`:                                  model.BackupKind,
		`C:\Users\ana\Documents\Virtual Machines\Ubuntu`:                  model.VirtualKind,
		`C:\Users\ana\Documents\site\node_modules\react`:                  model.BuildKind,
		`C:\Users\ana\Documents\site\dist`:                                model.BuildKind,
		`C:\Users\ana\source\app\target`:                                  model.BuildKind,
		`C:\Users\ana\source\app-wt`:                                      model.CheckoutKind,
		`C:\Users\ana\Downloads\Program Files`:                            model.DownloadKind,
		`E:\Program Files\Tool`:                                           model.ProgramKind,
		`C:\Users\ana\AppData\Local\Epic Games\EOSOverlay`:                model.AppKind,
		`C:\Users\ana\AppData\Local\Riot Games\Riot Client`:               model.AppKind,
		`C:\Users\ana\AppData\Local\ms-playwright\chromium-1200`:          model.PackageKind,
		`C:\Users\ana\iCloudDrive\Notes`:                                  model.PersonalKind,
		`E:\Backups\Program Files`:                                        model.BackupKind,
	} {
		if got := kindOfPath(path); got != want {
			t.Errorf("kindOfPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestClassifyUnitsAndFiles(t *testing.T) {
	steam := placeOf(`C:\Program Files (x86)\Steam\steamapps\common`, model.GameKind)
	if l := classify("Portal 2", steam, true); !l.unit || l.kind != model.GameKind || l.why == "" {
		t.Fatalf("a Steam game should start a labeled game: %+v", l)
	}
	if l := classify("Steam", placeOf(`C:\Program Files (x86)`, model.ProgramKind), true); !l.unit || l.kind != model.ProgramKind {
		t.Fatalf("a Program Files entry should start a labeled program: %+v", l)
	}
	if l := classify("Common Files", placeOf(`C:\Program Files`, model.ProgramKind), true); l.unit || l.kind != model.ProgramKind {
		t.Fatalf("shared components are not a program to uninstall: %+v", l)
	}
	if l := classify("bin", placeOf(`C:\Program Files\Tool`, model.ProgramKind), true); l.unit {
		t.Fatalf("folders inside a program are part of it: %+v", l)
	}
	for _, tc := range []struct {
		name string
		at   place
		want string
	}{
		{"hiberfil.sys", placeOf(`C:\`, model.FilesKind), model.SystemKind},
		{"ext4.vhdx", placeOf(`C:\Users\ana\AppData\Local\Packages\Ubuntu\LocalState`, model.AppKind), model.VirtualKind},
		{"docker_data.vhdx", placeOf(`C:\Users\ana\AppData\Local\Docker\wsl\disk`, model.AppKind), model.DockerKind},
		{"site-backup.zip", placeOf(`C:\work`, model.FilesKind), model.BackupKind},
		{"backup.vhdx", placeOf(`C:\work`, model.FilesKind), model.BackupKind},
		{"photos.zip", placeOf(`C:\work`, model.FilesKind), model.FilesKind},
		{"crash.dmp", placeOf(`C:\work`, model.FilesKind), model.PossibleCleanupKind},
		{"crash.dmp", placeOf(`C:\Games\Space`, model.GameKind), model.GameKind},
	} {
		if got := classify(tc.name, tc.at, false); got.kind != tc.want || got.unit {
			t.Errorf("classify(%q in %q) = %+v, want %q", tc.name, tc.at.lower, got, tc.want)
		}
	}
}

func TestContentLabel(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "CACHEDIR.TAG"), []byte(cacheSignature), 0600); err != nil {
		t.Fatal(err)
	}
	dir := func(name string) dirEntry { return dirEntry{Name: name, Attrs: attrsDirectory} }
	file := func(name string) dirEntry { return dirEntry{Name: name, Length: int64(len(cacheSignature))} }
	for _, tc := range []struct {
		entries []dirEntry
		kind    string
		want    string
	}{
		{[]dirEntry{file("README.md"), dir(".git")}, model.FilesKind, model.ProjectKind},
		{[]dirEntry{file(".git")}, model.PersonalKind, model.ProjectKind},
		{[]dirEntry{file("App.sln")}, model.FilesKind, model.ProjectKind},
		{[]dirEntry{dir(".git"), file("CACHEDIR.TAG")}, model.FilesKind, model.ProjectKind},
		{[]dirEntry{file(".build.info"), file("Game.exe")}, model.ProgramKind, model.GameKind},
		{[]dirEntry{file("UnityPlayer.dll"), file("package.json")}, model.FilesKind, model.ProjectKind},
		{[]dirEntry{dir(".egstore")}, model.FilesKind, model.GameKind},
		{[]dirEntry{dir(".git")}, model.ProgramKind, ""},
		{[]dirEntry{dir(".git")}, model.DownloadKind, ""},
		{[]dirEntry{file("CACHEDIR.TAG")}, model.BuildKind, model.CacheKind},
		{[]dirEntry{dir("CACHEDIR.TAG")}, model.FilesKind, ""},
	} {
		got, ok := contentLabel(root, tc.entries, tc.kind)
		if got.kind != tc.want || ok != (tc.want != "") {
			t.Errorf("contentLabel(%v, %q) = %+v, want %q", tc.entries, tc.kind, got, tc.want)
		}
	}
}

func TestCaptureLabelsCategoriesAndUnits(t *testing.T) {
	root := t.TempDir()
	write := func(path string, size int64) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mib := int64(units.MiB)
	write(`repo\.git\objects\pack.bin`, mib)
	write(`repo\src\main.go`, mib)
	write(`repo\node_modules\left-pad\index.js`, 2*mib)
	write(`repo\artifacts\CACHEDIR.TAG`, 0)
	if err := os.WriteFile(filepath.Join(root, `repo\artifacts\CACHEDIR.TAG`), []byte(cacheSignature), 0600); err != nil {
		t.Fatal(err)
	}
	tagBytes, _, err := FileAllocation(context.Background(), filepath.Join(root, `repo\artifacts\CACHEDIR.TAG`))
	if err != nil {
		t.Fatal(err)
	}
	write(`repo\artifacts\blob.bin`, 3*mib)
	write(`SteamLibrary\steamapps\common\Space Game\game.bin`, 4*mib)
	write(`SteamLibrary\steamapps\shadercache\620\shaders.bin`, mib)
	write(`AppData\Local\Programs\Launcher\launcher.bin`, mib)
	write(`AppData\Local\Programs\Launcher\steamapps\common\Nested Game\data.bin`, 2*mib)
	write(`Tools\Unity Game\UnityPlayer.dll`, mib)
	write(`notes.bin`, mib)

	s, err := capture(t, context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Coverage.Labels != model.LabelsVersion {
		t.Fatalf("capture should record its label rules: %+v", s.Coverage)
	}
	var folders, categories int64
	for _, item := range model.RecordsIn(s, model.Folders) {
		folders += item.Bytes
	}
	byKind := map[string]int64{}
	for _, item := range model.RecordsIn(s, model.Categories) {
		categories += item.Bytes
		byKind[item.Kind] = item.Bytes
	}
	if categories != folders {
		t.Fatalf("category totals %d should add up to the measured %d: %+v", categories, folders, byKind)
	}
	for kind, want := range map[string]int64{
		model.ProjectKind: 2 * mib, model.BuildKind: 2 * mib, model.CacheKind: 3*mib + tagBytes, model.PossibleCleanupKind: mib,
		model.GameKind: 7 * mib, model.ProgramKind: mib, model.FilesKind: mib,
	} {
		if byKind[kind] != want {
			t.Errorf("%s = %d, want %d (all: %+v)", kind, byKind[kind], want, byKind)
		}
	}

	units := map[string]model.Record{}
	for _, item := range model.RecordsIn(s, model.Labeled) {
		units[item.Name] = item
	}
	for name, want := range map[string]struct {
		kind  string
		bytes int64
	}{
		"repo":        {model.ProjectKind, 2 * mib},
		"Space Game":  {model.GameKind, 4 * mib},
		"Launcher":    {model.ProgramKind, mib},
		"Nested Game": {model.GameKind, 2 * mib},
		"Unity Game":  {model.GameKind, mib},
	} {
		got, ok := units[name]
		if !ok || got.Kind != want.kind || got.Bytes != want.bytes || got.Detail == "" {
			t.Errorf("labeled folder %s = %+v, want %s of %d bytes with a reason", name, got, want.kind, want.bytes)
		}
	}
	if len(units) != 5 {
		t.Errorf("unexpected labeled folders: %+v", units)
	}

	candidates := map[string]model.Record{}
	for _, item := range model.RecordsIn(s, model.Candidates) {
		candidates[filepath.Base(item.Path)] = item
	}
	for name, kind := range map[string]string{"node_modules": model.BuildKind, "artifacts": model.CacheKind, "shadercache": model.PossibleCleanupKind} {
		if got := candidates[name]; got.Kind != kind {
			t.Errorf("review candidate %s = %+v, want %s", name, got, kind)
		}
	}
	if got := candidates["artifacts"]; got.Detail == "" || got.Bytes != 3*mib+tagBytes {
		t.Errorf("a tagged cache should explain its label: %+v", got)
	}
	for _, item := range model.RecordsIn(s, model.Folders) {
		if item.Name == "repo" || filepath.Base(item.Path) == "repo" {
			if item.Kind != model.ProjectKind || item.Detail == "" {
				t.Errorf("the repository's folder reading should be relabeled: %+v", item)
			}
		}
	}
}

func TestCleanupCandidatesPartitionTheirCategories(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string][]byte{
		`worktrees\project\copy.bin`:                    make([]byte, units.MiB),
		`worktrees\project\cache\CACHEDIR.TAG`:          []byte(cacheSignature),
		`worktrees\project\cache\blob.bin`:              make([]byte, units.MiB/2),
		`worktrees\project\node_modules\dependency.bin`: make([]byte, units.MiB),
		`Client work\cache\unique.bin`:                  make([]byte, units.MiB),
		`Important project\CACHEDIR.TAG`:                {},
		`Important project\original.bin`:                make([]byte, units.MiB),
	} {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := capture(t, context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	totals, _ := model.RemovalTotals(s)
	tagBytes, _, err := FileAllocation(context.Background(), filepath.Join(root, `worktrees\project\cache\CACHEDIR.TAG`))
	if err != nil {
		t.Fatal(err)
	}
	safeBytes := units.MiB/2 + tagBytes
	if totals[model.SafeToClear] != safeBytes || totals[model.CheckFirst] != 3*units.MiB || totals[model.Keep] != units.MiB {
		t.Fatalf("unexpected totals: %+v", totals)
	}
	bytes := make(map[string]int64)
	for _, item := range model.RecordsIn(s, model.Candidates) {
		rel, _ := filepath.Rel(root, item.Path)
		bytes[rel] = item.Bytes
	}
	for path, want := range map[string]int64{
		`worktrees`: units.MiB, `worktrees\project\cache`: safeBytes,
		`worktrees\project\node_modules`: units.MiB, `Client work\cache`: units.MiB,
	} {
		if bytes[path] != want {
			t.Fatalf("candidate %s got %d bytes, want %d: %+v", path, bytes[path], want, bytes)
		}
	}
	if len(bytes) != 4 {
		t.Fatalf("unexpected candidates: %+v", bytes)
	}
}

func TestProjectScopeLabelsItsContents(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{`.git\HEAD`, `node_modules\x\index.js`} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, units.MiB), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := capture(t, context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range model.RecordsIn(s, model.Folders) {
		want := model.ProjectKind
		if filepath.Base(item.Path) == "node_modules" {
			want = model.BuildKind
		}
		if item.Kind != want {
			t.Errorf("%s = %s, want %s", item.Path, item.Kind, want)
		}
	}
}

func TestMarkerNamesDecodeWithoutAllocating(t *testing.T) {
	utf16 := func(s string) []byte {
		var raw []byte
		for _, r := range s {
			raw = append(raw, byte(r), byte(r>>8))
		}
		return raw
	}
	tag, gog, other := utf16("CACHEDIR.TAG"), utf16("goggame-1207658924.info"), utf16("IMG_2048.JPG")
	if !markerName(tag) || !markerName(gog) || markerName(other) || markerName(utf16("é.sln")) {
		t.Fatal("marker names should match without case and skip other files")
	}
	if allocs := testing.AllocsPerRun(100, func() { markerName(tag); markerName(other) }); allocs != 0 {
		t.Fatalf("markerName allocated %.1f times per run", allocs)
	}
}
