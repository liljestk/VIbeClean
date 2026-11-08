package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"golang.org/x/sys/windows/registry"
)

const (
	dockerScope  = "Docker"
	projectLabel = "com.docker.compose.project"
	builderGroup = "@buildx"
	sharedGroup  = "@shared"
	standalone   = "@standalone"
	unassigned   = "@unassigned"
)

func dockerDisks() []string {
	var roots, candidates []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		roots = append(roots, filepath.Join(local, "Docker", "wsl"))
	}
	if roaming := os.Getenv("APPDATA"); roaming != "" {
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
	}
	wsl, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Lxss`, registry.ENUMERATE_SUB_KEYS)
	if err == nil {
		keys, _ := wsl.ReadSubKeyNames(-1)
		for _, name := range keys {
			distro, err := registry.OpenKey(wsl, name, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			label, _, _ := distro.GetStringValue("DistributionName")
			base, _, _ := distro.GetStringValue("BasePath")
			vhd, _, _ := distro.GetStringValue("VhdFileName")
			distro.Close()
			if (label != "docker-desktop" && label != "docker-desktop-data") || base == "" {
				continue
			}
			base = strings.TrimPrefix(base, "\\\\?\\")
			root := base
			if strings.EqualFold(filepath.Base(base), "main") {
				root = filepath.Dir(base)
			}
			roots = append(roots, root)
			if vhd == "" {
				vhd = "ext4.vhdx"
			}
			if filepath.Base(vhd) == vhd && strings.EqualFold(filepath.Ext(vhd), ".vhdx") {
				candidates = append(candidates, filepath.Join(base, vhd))
			}
		}
		wsl.Close()
	}
	for _, root := range roots {
		root = strings.TrimPrefix(root, "\\\\?\\")
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
		if _, err := localFolder(filepath.Dir(path)); err != nil {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			paths[normalizedKey(path)] = path
		}
	}
	var found []string
	for _, path := range paths {
		found = append(found, path)
	}
	slices.Sort(found)
	return found
}

func hostDiskRecord(ctx context.Context, phase string) record {
	started := time.Now()
	result := record{Series: dockerSum, Key: "host", Name: "Windows disk", Kind: "Host disk",
		Bytes: -1, Reclaimable: -1}
	var paths []string
	reader := new(directoryReader)
	disks := dockerDisks()
	var readErr error
	for _, path := range disks {
		err := reader.entries(ctx, filepath.Dir(path), 0, func(entry dirEntry) {
			if !entry.directory() && !entry.link() && strings.EqualFold(entry.Name, filepath.Base(path)) {
				result.Bytes = max(0, result.Bytes) + entry.Bytes
				paths = append(paths, path)
			}
		})
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
	logDuration(ctx, "docker.host_disk", started, readErr, slog.String("phase", phase),
		slog.Int("candidates", len(disks)), slog.Int("disks_measured", len(paths)))
	return result
}

var namedPipe = regexp.MustCompile("(?i)^npipe:////\\./pipe/([a-z0-9_.-]+)$")

func dockerPipes(ctx context.Context) []string {
	started := time.Now()
	pipes := []string{"dockerDesktopLinuxEngine", "docker_engine"}
	var probeErr error
	usedContext := false
	if cli, err := exec.LookPath("docker.exe"); err == nil {
		probe, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probe, cli, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		data, err := cmd.Output()
		probeErr = err
		if err == nil {
			if match := namedPipe.FindStringSubmatch(strings.TrimSpace(string(data))); len(match) == 2 {
				pipes = append([]string{match[1]}, pipes...)
				usedContext = true
			}
		} else if probe.Err() != nil {
			probeErr = probe.Err()
		}
	}
	pipes = slices.CompactFunc(pipes, strings.EqualFold)
	logDuration(ctx, "docker.discover", started, probeErr, slog.Bool("context_pipe", usedContext), slog.Int("candidates", len(pipes)))
	return pipes
}

func dockerJSON(ctx context.Context, client *http.Client, path string, value any) (err error) {
	const responseLimit = 128 << 20
	started := time.Now()
	stage, status := "request", 0
	var headerTime, decodeTime time.Duration
	var readBytes int64
	defer func() {
		logDuration(ctx, "docker.api", started, err, slog.String("endpoint", path), slog.String("stage", stage),
			slog.Int("http_status", status), slog.Int64("response_bytes", readBytes),
			slog.Float64("headers_ms", milliseconds(headerTime)), slog.Float64("decode_ms", milliseconds(decodeTime)))
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
		return fmt.Errorf("Docker returned HTTP %d for %s", response.StatusCode, path)
	}
	stage = "decode"
	decodeStarted := time.Now()
	body := &io.LimitedReader{R: response.Body, N: responseLimit}
	err = json.NewDecoder(body).Decode(value)
	decodeTime = time.Since(decodeStarted)
	readBytes = responseLimit - body.N
	return err
}

func captureDocker(ctx context.Context) (saved snapshot, err error) {
	started := time.Now()
	stage, attempts := "probe_engine", 0
	var groupTime time.Duration
	defer func() {
		logDuration(ctx, "docker.capture", started, err, slog.String("stage", stage), slog.Int("pipe_attempts", attempts),
			slog.Int("readings", len(saved.Records)), slog.Float64("group_ms", milliseconds(groupTime)))
	}()
	for _, pipe := range dockerPipes(ctx) {
		attempts++
		transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, "\\\\.\\pipe\\"+pipe)
		}}
		client := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
		var version struct {
			OS, Version string
			Platform    struct{ Name string }
		}
		probe, cancel := context.WithTimeout(ctx, 4*time.Second)
		err := dockerJSON(probe, client, "/version", &version)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return snapshot{}, ctx.Err()
			}
			continue
		}
		stage = "disk_usage"
		var data dockerData
		if err := dockerJSON(ctx, client, "/system/df", &data); err != nil {
			return snapshot{}, err
		}
		label := version.Platform.Name
		if label == "" {
			label = "Docker Engine " + version.Version
		}
		captured := time.Now().UnixMilli()
		stage = "group_usage"
		groupStarted := time.Now()
		records := dockerRecords(data)
		groupTime = time.Since(groupStarted)
		stage = "host_disk"
		return snapshot{
			Scope: "docker-v1|" + strings.ToLower(pipe) + "|" + version.OS,
			Label: dockerScope, Captured: captured,
			Coverage: coverage{ScopeKind: "docker", Engine: label},
			Records:  append(records, hostDiskRecord(ctx, "capture")),
		}, ctx.Err()
	}
	return snapshot{}, errors.New("local Docker Engine is unavailable. Start Docker Desktop and refresh")
}

type dockerContainer struct {
	ID, Image, State string
	SizeRw           int64
	Names            []string
	Labels           map[string]string
	Mounts           []struct{ Type, Name string }
}

type dockerVolume struct {
	Name, Driver string
	Labels       map[string]string
	UsageData    *struct{ Size, RefCount int64 }
}

type dockerImage struct {
	ID                           string
	RepoTags                     []string
	Size, SharedSize, Containers int64
}

type dockerCache struct {
	ID, Type, LastUsedAt string
	Size                 int64
	InUse, Shared        bool
}

type engineUsage[T any] struct {
	Items       []T
	TotalSize   *int64
	Reclaimable *int64
}

type dockerData struct {
	LayersSize      int64
	Images          []dockerImage
	Containers      []dockerContainer
	Volumes         []dockerVolume
	BuildCache      []dockerCache
	ImageUsage      engineUsage[dockerImage]
	ContainerUsage  engineUsage[dockerContainer]
	VolumeUsage     engineUsage[dockerVolume]
	BuildCacheUsage engineUsage[dockerCache]
}

func useItems[T any](report engineUsage[T], legacy []T) []T {
	if report.Items != nil {
		return report.Items
	}
	return legacy
}

func countText(count int, name string) string {
	if count != 1 {
		name += "s"
	}
	return fmt.Sprintf("%d %s", count, name)
}

func dockerGroup(name string) string {
	switch name {
	case builderGroup:
		return "Buildx builders"
	case sharedGroup:
		return "Shared volumes"
	case standalone:
		return "Standalone containers"
	case unassigned:
		return "Unassigned volumes"
	}
	return name
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(len(id), 12)]
}

func dockerRecords(data dockerData) []record {
	var result []record
	type group struct {
		bytes                        int64
		containers, volumes, unknown int
	}
	groups := make(map[string]group)
	mountedBy := make(map[string]map[string]bool)
	references := make(map[string]int)
	updateGroup := func(item record) {
		project := groups[item.Group]
		if item.Series == containers {
			project.containers++
		} else {
			project.volumes++
		}
		if item.Bytes >= 0 {
			project.bytes += item.Bytes
		} else {
			project.unknown++
		}
		groups[item.Group] = project
		result = append(result, item)
	}
	var rwBytes, volumeBytes int64
	var partialContainers, partialVolumes bool
	for _, container := range useItems(data.ContainerUsage, data.Containers) {
		name := shortID(container.ID)
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		project := container.Labels[projectLabel]
		if project == "" {
			project = standalone
			if strings.HasPrefix(name, "buildx_buildkit_") {
				for _, mount := range container.Mounts {
					if mount.Type == "volume" && mount.Name == name+"_state" {
						project = builderGroup
						break
					}
				}
			}
		}
		volumeSet := make(map[string]bool)
		binds := 0
		for _, mount := range container.Mounts {
			switch mount.Type {
			case "volume":
				if mount.Name != "" {
					volumeSet[mount.Name] = true
				}
			case "bind":
				binds++
			}
		}
		var attached []string
		for volume := range volumeSet {
			if mountedBy[volume] == nil {
				mountedBy[volume] = make(map[string]bool)
			}
			mountedBy[volume][project] = true
			references[volume]++
			attached = append(attached, volume)
		}
		slices.Sort(attached)
		detail := "Image: " + container.Image + "\r\nGroup: " + dockerGroup(project)
		if len(attached) > 0 {
			detail += "\r\nVolumes: " + strings.Join(attached, ", ")
		}
		if binds > 0 {
			detail += "\r\n" + countText(binds, "bind mount") + " outside the writable layer."
		}
		state := container.State
		idle := state == "exited" || state == "dead" || state == "created"
		updateGroup(record{Series: containers, Key: container.ID, Name: name, Kind: "Container",
			Bytes: container.SizeRw, Group: project, Activity: state, Detail: detail, Idle: idle})
		rwBytes += max(0, container.SizeRw)
		partialContainers = partialContainers || container.SizeRw < 0
	}
	for _, volume := range useItems(data.VolumeUsage, data.Volumes) {
		associated := mountedBy[volume.Name]
		if associated == nil {
			associated = make(map[string]bool)
		}
		if label := volume.Labels[projectLabel]; label != "" {
			associated[label] = true
		}
		project := unassigned
		var labels []string
		for key := range associated {
			project = key
			labels = append(labels, dockerGroup(key))
		}
		slices.Sort(labels)
		if len(associated) > 1 {
			project = sharedGroup
		}
		if project == unassigned && strings.HasPrefix(volume.Name, "buildx_buildkit_") && strings.HasSuffix(volume.Name, "_state") {
			project = builderGroup
		}
		size, refs := int64(-1), references[volume.Name]
		if volume.UsageData != nil {
			size = volume.UsageData.Size
			if volume.UsageData.RefCount >= 0 {
				refs = int(volume.UsageData.RefCount)
			}
		}
		state := countText(refs, "reference")
		if refs == 0 {
			state = "Unreferenced"
		}
		detail := "Driver: " + volume.Driver + "\r\nGroup: " + dockerGroup(project)
		if len(labels) > 1 {
			detail += "\r\nAssociated groups: " + strings.Join(labels, ", ")
		}
		if project == builderGroup {
			detail += "\r\nBuildx stores builder state and cache in this volume."
		} else {
			detail += "\r\nVolumes can hold databases or other persistent data."
		}
		updateGroup(record{Series: volumes, Key: volume.Name, Name: volume.Name, Kind: "Volume",
			Bytes: size, Activity: state, Detail: detail, Group: project, Idle: refs == 0})
		volumeBytes += max(0, size)
		partialVolumes = partialVolumes || size < 0
	}
	for key, group := range groups {
		var counts []string
		if group.containers > 0 {
			counts = append(counts, countText(group.containers, "container"))
		}
		if group.volumes > 0 {
			counts = append(counts, countText(group.volumes, "volume"))
		}
		detail := "Writable container layers and assigned volumes."
		if key == builderGroup {
			detail = "Buildx builder state is included in Docker volume usage."
		}
		if group.unknown > 0 {
			detail += "\r\nSizes unavailable for " + countText(group.unknown, "item") + "."
		}
		size := group.bytes
		if group.unknown == group.containers+group.volumes {
			size = -1
		}
		result = append(result, record{Series: projects, Key: key, Name: dockerGroup(key), Kind: "Project",
			Bytes: size, Activity: strings.Join(counts, " · "), Detail: detail, Group: key, Partial: group.unknown != 0})
	}
	for _, image := range useItems(data.ImageUsage, data.Images) {
		id := shortID(image.ID)
		name := "Untagged · " + id
		if len(image.RepoTags) > 0 && image.RepoTags[0] != "<none>:<none>" {
			name = image.RepoTags[0]
		}
		state := countText(int(image.Containers), "reference")
		if image.Containers == 0 {
			state = "Unreferenced"
		} else if image.Containers < 0 {
			state = "Unknown"
		}
		detail := "Image ID: " + id + "\r\nListed size includes shared layers."
		if image.Size >= 0 && image.SharedSize >= 0 {
			detail += "\r\nUnique size: " + sizeText(max(0, image.Size-image.SharedSize))
		}
		if len(image.RepoTags) > 1 {
			detail += "\r\nTags: " + strings.Join(image.RepoTags, ", ")
		}
		result = append(result, record{Series: images, Key: image.ID, Name: name, Kind: "Image",
			Bytes: image.Size, Activity: state, Detail: detail, Idle: image.Containers == 0})
	}
	for _, cache := range useItems(data.BuildCacheUsage, data.BuildCache) {
		state := "Idle"
		if cache.InUse {
			state = "In use"
		} else if cache.Shared {
			state = "Shared"
		}
		detail := "Build cache ID: " + cache.ID
		if len(cache.LastUsedAt) >= 10 {
			detail += "\r\nLast used: " + cache.LastUsedAt[:10]
		}
		result = append(result, record{Series: buildCache, Key: cache.ID, Name: cache.Type + " · " + shortID(cache.ID),
			Kind: buildCache, Bytes: cache.Size, Activity: state, Detail: detail, Idle: !cache.Shared && !cache.InUse})
	}
	result = append(result,
		dockerTotal(images, data.ImageUsage.TotalSize, data.ImageUsage.Reclaimable, data.LayersSize, false),
		dockerTotal(containers, data.ContainerUsage.TotalSize, data.ContainerUsage.Reclaimable, rwBytes, partialContainers),
		dockerTotal(volumes, data.VolumeUsage.TotalSize, data.VolumeUsage.Reclaimable, volumeBytes, partialVolumes),
		dockerTotal(buildCache, data.BuildCacheUsage.TotalSize, data.BuildCacheUsage.Reclaimable, -1, false),
	)
	return result
}

func dockerTotal(name string, size, reclaimable *int64, fallback int64, partial bool) record {
	item := record{Series: dockerSum, Key: name, Name: name, Kind: "Engine usage", Bytes: fallback, Reclaimable: -1}
	if size != nil {
		item.Bytes = *size
	}
	item.Partial = size == nil && partial
	if reclaimable != nil {
		item.Reclaimable = *reclaimable
		item.Activity = sizeText(*reclaimable) + " reclaimable"
	}
	item.Detail = "Docker Engine reported usage. Images and build cache can share data."
	return item
}
