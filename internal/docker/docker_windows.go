// Package docker measures the local Docker Engine: images, containers,
// volumes and build cache from the engine API, and the Windows virtual disks
// that hold them.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/scan"
)

const (
	responseLimit = 128 << 20
	probeTimeout  = 4 * time.Second
	hostKey       = "host"
)

var namedPipe = regexp.MustCompile(`(?i)^npipe:////\./pipe/([a-z0-9_.-]+)$`)

// Disks finds the Docker Desktop and WSL virtual disk files on this PC.
func Disks() []string {
	wslRoots, candidates := wslLocations()
	roots := append(settingsRoots(), wslRoots...)
	for _, root := range roots {
		root = strings.TrimPrefix(root, `\\?\`)
		if strings.EqualFold(filepath.Ext(root), ".vhdx") {
			candidates = append(candidates, root)
			continue
		}
		for _, name := range []string{`disk\docker_data.vhdx`, `main\ext4.vhdx`, `data\ext4.vhdx`,
			"docker_data.vhdx", "ext4.vhdx", "DockerDesktop.vhdx", "DockerDesktopVM.vhdx"} {
			candidates = append(candidates, filepath.Join(root, name))
		}
	}
	paths := make(map[string]string)
	for _, path := range candidates {
		if _, err := scan.LocalFolder(filepath.Dir(path)); err != nil {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			paths[model.NormalizedKey(path)] = path
		}
	}
	var found []string
	for _, path := range paths {
		found = append(found, path)
	}
	slices.Sort(found)
	return found
}

// settingsRoots reads the Docker Desktop data folders from its settings files.
func settingsRoots() []string {
	var roots []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		roots = append(roots, filepath.Join(local, "Docker", "wsl"))
	}
	roaming := os.Getenv("APPDATA")
	if roaming == "" {
		return roots
	}
	for _, name := range []string{"settings-store.json", "settings.json"} {
		data, err := os.ReadFile(filepath.Join(roaming, "Docker", name))
		if err != nil {
			continue
		}
		var config map[string]json.RawMessage
		if json.Unmarshal(data, &config) != nil {
			continue
		}
		for _, key := range []string{"dataFolder", "diskImageLocation", "wslDiskImageLocation"} {
			var value string
			if json.Unmarshal(config[key], &value) == nil && value != "" {
				roots = append(roots, value)
			}
		}
	}
	return roots
}

// wslLocations reads the Docker Desktop WSL distributions from the registry
// and returns their data folders and the disk files they name.
func wslLocations() (roots, disks []string) {
	wsl, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Lxss`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, nil
	}
	defer wsl.Close()
	keys, _ := wsl.ReadSubKeyNames(-1)
	for _, name := range keys {
		root, disk := wslDistribution(wsl, name)
		if root != "" {
			roots = append(roots, root)
		}
		if disk != "" {
			disks = append(disks, disk)
		}
	}
	return roots, disks
}

func wslDistribution(parent registry.Key, name string) (root, disk string) {
	distro, err := registry.OpenKey(parent, name, registry.QUERY_VALUE)
	if err != nil {
		return "", ""
	}
	defer distro.Close()
	label, _, _ := distro.GetStringValue("DistributionName")
	base, _, _ := distro.GetStringValue("BasePath")
	vhd, _, _ := distro.GetStringValue("VhdFileName")
	if (label != "docker-desktop" && label != "docker-desktop-data") || base == "" {
		return "", ""
	}
	base = strings.TrimPrefix(base, `\\?\`)
	root = base
	if strings.EqualFold(filepath.Base(base), "main") {
		root = filepath.Dir(base)
	}
	if vhd == "" {
		vhd = "ext4.vhdx"
	}
	if filepath.Base(vhd) == vhd && strings.EqualFold(filepath.Ext(vhd), ".vhdx") {
		disk = filepath.Join(base, vhd)
	}
	return root, disk
}

func hostDiskRecord(ctx context.Context, phase string) model.Record {
	started := time.Now()
	result := model.Record{Series: model.DockerTotals, Key: hostKey, Name: "Windows disk", Kind: "Host disk",
		Bytes: -1, Reclaimable: -1}
	var paths []string
	disks := Disks()
	var readErr error
	for _, path := range disks {
		size, found, err := scan.FileAllocation(ctx, path)
		if found {
			result.Bytes = max(0, result.Bytes) + size
			paths = append(paths, path)
		}
		if readErr == nil {
			readErr = err
		}
	}
	if len(paths) == 1 {
		result.Path = paths[0]
	}
	result.Detail = "Allocated Windows disk files. Docker resource totals are a separate measurement."
	if len(paths) != 0 {
		result.Detail += "\r\n" + strings.Join(paths, "\r\n")
	}
	diag.Duration(ctx, "docker.host_disk", started, readErr, slog.String("phase", phase),
		slog.Int("candidates", len(disks)), slog.Int("disks_measured", len(paths)))
	return result
}

// pipes lists the named pipes to try, the current Docker context's pipe first.
func pipes(ctx context.Context) []string {
	started := time.Now()
	candidates := []string{"dockerDesktopLinuxEngine", "docker_engine"}
	var probeErr error
	usedContext := false
	if cli, err := exec.LookPath("docker.exe"); err == nil {
		probe, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		cmd := exec.CommandContext(probe, cli, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		data, err := cmd.Output()
		probeErr = err
		if err == nil {
			if match := namedPipe.FindStringSubmatch(strings.TrimSpace(string(data))); len(match) == 2 {
				candidates = append([]string{match[1]}, candidates...)
				usedContext = true
			}
		} else if probe.Err() != nil {
			probeErr = probe.Err()
		}
	}
	candidates = unique(candidates)
	diag.Duration(ctx, "docker.discover", started, probeErr, slog.Bool("context_pipe", usedContext), slog.Int("candidates", len(candidates)))
	return candidates
}

// unique keeps the first of each pipe name, ignoring case.
func unique(names []string) []string {
	var result []string
	for _, name := range names {
		if !slices.ContainsFunc(result, func(seen string) bool { return strings.EqualFold(seen, name) }) {
			result = append(result, name)
		}
	}
	return result
}

func getJSON(ctx context.Context, client *http.Client, path string, value any) (err error) {
	started := time.Now()
	stage, status := "request", 0
	var headerTime, decodeTime time.Duration
	var readBytes int64
	defer func() {
		diag.Duration(ctx, "docker.api", started, err, slog.String("endpoint", path), slog.String("stage", stage),
			slog.Int("http_status", status), slog.Int64("response_bytes", readBytes),
			slog.Float64("headers_ms", diag.Milliseconds(headerTime)), slog.Float64("decode_ms", diag.Milliseconds(decodeTime)))
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	headerTime = time.Since(started)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	status = response.StatusCode
	if response.StatusCode != http.StatusOK {
		stage = "response_status"
		return fmt.Errorf("docker returned HTTP %d for %s", response.StatusCode, path)
	}
	stage = "decode"
	decodeStarted := time.Now()
	body := &io.LimitedReader{R: response.Body, N: responseLimit}
	err = json.NewDecoder(body).Decode(value)
	decodeTime = time.Since(decodeStarted)
	readBytes = responseLimit - body.N
	return err
}

// Capture measures the first local engine that answers and returns an unsaved snapshot.
func Capture(ctx context.Context) (saved model.Snapshot, err error) {
	started := time.Now()
	stage, attempts := "probe_engine", 0
	var groupTime time.Duration
	defer func() {
		diag.Duration(ctx, "docker.capture", started, err, slog.String("stage", stage), slog.Int("pipe_attempts", attempts),
			slog.Int("readings", len(saved.Records)), slog.Float64("group_ms", diag.Milliseconds(groupTime)))
	}()
	for _, pipe := range pipes(ctx) {
		attempts++
		client := pipeClient(pipe)
		var version struct {
			OS, Version string
			Platform    struct{ Name string }
		}
		probe, cancel := context.WithTimeout(ctx, probeTimeout)
		err := getJSON(probe, client, "/version", &version)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return model.Snapshot{}, ctx.Err()
			}
			continue
		}
		stage = "disk_usage"
		var data usage
		if err := getJSON(ctx, client, "/system/df", &data); err != nil {
			return model.Snapshot{}, err
		}
		label := version.Platform.Name
		if label == "" {
			label = "Docker Engine " + version.Version
		}
		captured := time.Now().UnixMilli()
		stage = "group_usage"
		groupStarted := time.Now()
		items := records(data)
		groupTime = time.Since(groupStarted)
		stage = "host_disk"
		return model.Snapshot{
			Scope: "docker-v1|" + strings.ToLower(pipe) + "|" + version.OS,
			Label: model.DockerScope, Captured: captured,
			Coverage: model.Coverage{ScopeKind: model.ScopeDocker, Engine: label},
			Records:  append(items, hostDiskRecord(ctx, "capture")),
		}, ctx.Err()
	}
	return model.Snapshot{}, model.Unavailable("local Docker Engine is unavailable. Start Docker Desktop and refresh")
}

func pipeClient(pipe string) *http.Client {
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, `\\.\pipe\`+pipe)
	}}
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute}
}
