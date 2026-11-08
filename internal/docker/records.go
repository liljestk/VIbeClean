package docker

import (
	"fmt"
	"slices"
	"strings"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const (
	projectLabel = "com.docker.compose.project"
	builderGroup = "@buildx"
	sharedGroup  = "@shared"
	standalone   = "@standalone"
	unassigned   = "@unassigned"
	buildxPrefix = "buildx_buildkit_"
	buildxSuffix = "_state"
)

type container struct {
	ID, Image, State string
	SizeRw           int64
	Names            []string
	Labels           map[string]string
	Mounts           []struct{ Type, Name string }
}

type volume struct {
	Name, Driver string
	Labels       map[string]string
	UsageData    *struct{ Size, RefCount int64 }
}

type image struct {
	ID                           string
	RepoTags                     []string
	Size, SharedSize, Containers int64
}

type cacheEntry struct {
	ID, Type, LastUsedAt string
	Size                 int64
	InUse, Shared        bool
}

type engineUsage[T any] struct {
	Items       []T
	TotalSize   *int64
	Reclaimable *int64
}

// usage is the /system/df response. Newer engines report the *Usage objects;
// older ones report the plain lists.
type usage struct {
	LayersSize      int64
	Images          []image
	Containers      []container
	Volumes         []volume
	BuildCache      []cacheEntry
	ImageUsage      engineUsage[image]
	ContainerUsage  engineUsage[container]
	VolumeUsage     engineUsage[volume]
	BuildCacheUsage engineUsage[cacheEntry]
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

func groupName(name string) string {
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

type group struct {
	bytes                        int64
	containers, volumes, unknown int
}

// grouping assigns containers and volumes to Compose projects and keeps
// the per-project totals.
type grouping struct {
	result            []model.Record
	groups            map[string]group
	mountedBy         map[string]map[string]bool
	references        map[string]int
	rwBytes, volBytes int64
	partialRW         bool
	partialVolumes    bool
}

func (g *grouping) add(item model.Record) {
	project := g.groups[item.Group]
	if item.Series == model.Containers {
		project.containers++
	} else {
		project.volumes++
	}
	if item.Bytes >= 0 {
		project.bytes += item.Bytes
	} else {
		project.unknown++
	}
	g.groups[item.Group] = project
	g.result = append(g.result, item)
}

// records turns engine usage into readings.
func records(data usage) []model.Record {
	g := &grouping{groups: map[string]group{}, mountedBy: map[string]map[string]bool{}, references: map[string]int{}}
	for _, c := range useItems(data.ContainerUsage, data.Containers) {
		g.container(c)
	}
	for _, v := range useItems(data.VolumeUsage, data.Volumes) {
		g.volume(v)
	}
	g.projects()
	for _, img := range useItems(data.ImageUsage, data.Images) {
		g.result = append(g.result, imageRecord(img))
	}
	for _, entry := range useItems(data.BuildCacheUsage, data.BuildCache) {
		g.result = append(g.result, cacheRecord(entry))
	}
	return append(g.result,
		total(model.Images, data.ImageUsage.TotalSize, data.ImageUsage.Reclaimable, data.LayersSize, false),
		total(model.Containers, data.ContainerUsage.TotalSize, data.ContainerUsage.Reclaimable, g.rwBytes, g.partialRW),
		total(model.Volumes, data.VolumeUsage.TotalSize, data.VolumeUsage.Reclaimable, g.volBytes, g.partialVolumes),
		total(model.BuildCache, data.BuildCacheUsage.TotalSize, data.BuildCacheUsage.Reclaimable, -1, false),
	)
}

func containerProject(c container, name string) string {
	if project := c.Labels[projectLabel]; project != "" {
		return project
	}
	if strings.HasPrefix(name, buildxPrefix) {
		for _, mount := range c.Mounts {
			if mount.Type == "volume" && mount.Name == name+buildxSuffix {
				return builderGroup
			}
		}
	}
	return standalone
}

func (g *grouping) container(c container) {
	name := shortID(c.ID)
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	project := containerProject(c, name)
	mounted := make(map[string]bool)
	binds := 0
	for _, mount := range c.Mounts {
		switch {
		case mount.Type == "volume" && mount.Name != "":
			mounted[mount.Name] = true
		case mount.Type == "bind":
			binds++
		}
	}
	var attached []string
	for volumeName := range mounted {
		if g.mountedBy[volumeName] == nil {
			g.mountedBy[volumeName] = make(map[string]bool)
		}
		g.mountedBy[volumeName][project] = true
		g.references[volumeName]++
		attached = append(attached, volumeName)
	}
	slices.Sort(attached)
	detail := "Image: " + c.Image + "\r\nGroup: " + groupName(project)
	if len(attached) > 0 {
		detail += "\r\nVolumes: " + strings.Join(attached, ", ")
	}
	if binds > 0 {
		detail += "\r\n" + countText(binds, "bind mount") + " outside the writable layer."
	}
	idle := c.State == "exited" || c.State == "dead" || c.State == "created"
	g.add(model.Record{Series: model.Containers, Key: c.ID, Name: name, Kind: "Container",
		Bytes: c.SizeRw, Group: project, Activity: c.State, Detail: detail, Idle: idle})
	g.rwBytes += max(0, c.SizeRw)
	g.partialRW = g.partialRW || c.SizeRw < 0
}

// volumeProject assigns a volume to the one project that uses it, to the
// shared group when several do, or to the Buildx group by its name.
func (g *grouping) volumeProject(v volume) (string, []string) {
	associated := make(map[string]bool)
	for project := range g.mountedBy[v.Name] {
		associated[project] = true
	}
	if label := v.Labels[projectLabel]; label != "" {
		associated[label] = true
	}
	project := unassigned
	var labels []string
	for key := range associated {
		project = key
		labels = append(labels, groupName(key))
	}
	slices.Sort(labels)
	if len(associated) > 1 {
		project = sharedGroup
	}
	if project == unassigned && strings.HasPrefix(v.Name, buildxPrefix) && strings.HasSuffix(v.Name, buildxSuffix) {
		project = builderGroup
	}
	return project, labels
}

func (g *grouping) volume(v volume) {
	project, labels := g.volumeProject(v)
	size, refs := int64(-1), g.references[v.Name]
	if v.UsageData != nil {
		size = v.UsageData.Size
		if v.UsageData.RefCount >= 0 {
			refs = int(v.UsageData.RefCount)
		}
	}
	state := countText(refs, "reference")
	if refs == 0 {
		state = "Unreferenced"
	}
	detail := "Driver: " + v.Driver + "\r\nGroup: " + groupName(project)
	if len(labels) > 1 {
		detail += "\r\nAssociated groups: " + strings.Join(labels, ", ")
	}
	if project == builderGroup {
		detail += "\r\nBuildx stores builder state and cache in this volume."
	} else {
		detail += "\r\nVolumes can hold databases or other persistent data."
	}
	g.add(model.Record{Series: model.Volumes, Key: v.Name, Name: v.Name, Kind: "Volume",
		Bytes: size, Activity: state, Detail: detail, Group: project, Idle: refs == 0})
	g.volBytes += max(0, size)
	g.partialVolumes = g.partialVolumes || size < 0
}

func (g *grouping) projects() {
	for key, project := range g.groups {
		var counts []string
		if project.containers > 0 {
			counts = append(counts, countText(project.containers, "container"))
		}
		if project.volumes > 0 {
			counts = append(counts, countText(project.volumes, "volume"))
		}
		detail := "Writable container layers and assigned volumes."
		if key == builderGroup {
			detail = "Buildx builder state is included in Docker volume usage."
		}
		if project.unknown > 0 {
			detail += "\r\nSizes unavailable for " + countText(project.unknown, "item") + "."
		}
		size := project.bytes
		if project.unknown == project.containers+project.volumes {
			size = -1
		}
		g.result = append(g.result, model.Record{Series: model.Projects, Key: key, Name: groupName(key), Kind: "Project",
			Bytes: size, Activity: strings.Join(counts, " · "), Detail: detail, Group: key, Partial: project.unknown != 0})
	}
}

func imageRecord(img image) model.Record {
	id := shortID(img.ID)
	name := "Untagged · " + id
	if len(img.RepoTags) > 0 && img.RepoTags[0] != "<none>:<none>" {
		name = img.RepoTags[0]
	}
	state := countText(int(img.Containers), "reference")
	switch {
	case img.Containers == 0:
		state = "Unreferenced"
	case img.Containers < 0:
		state = "Unknown"
	}
	detail := "Image ID: " + id + "\r\nListed size includes shared layers."
	if img.Size >= 0 && img.SharedSize >= 0 {
		detail += "\r\nUnique size: " + units.Size(max(0, img.Size-img.SharedSize))
	}
	if len(img.RepoTags) > 1 {
		detail += "\r\nTags: " + strings.Join(img.RepoTags, ", ")
	}
	return model.Record{Series: model.Images, Key: img.ID, Name: name, Kind: "Image",
		Bytes: img.Size, Activity: state, Detail: detail, Idle: img.Containers == 0}
}

func cacheRecord(entry cacheEntry) model.Record {
	state := "Idle"
	switch {
	case entry.InUse:
		state = "In use"
	case entry.Shared:
		state = "Shared"
	}
	detail := "Build cache ID: " + entry.ID
	if len(entry.LastUsedAt) >= 10 {
		detail += "\r\nLast used: " + entry.LastUsedAt[:10]
	}
	return model.Record{Series: model.BuildCache, Key: entry.ID, Name: entry.Type + " · " + shortID(entry.ID),
		Kind: model.BuildCache, Bytes: entry.Size, Activity: state, Detail: detail, Idle: !entry.Shared && !entry.InUse}
}

func total(name string, size, reclaimable *int64, fallback int64, partial bool) model.Record {
	item := model.Record{Series: model.DockerTotals, Key: name, Name: name, Kind: "Engine usage", Bytes: fallback, Reclaimable: -1}
	if size != nil {
		item.Bytes = *size
	}
	item.Partial = size == nil && partial
	if reclaimable != nil {
		item.Reclaimable = *reclaimable
		item.Activity = units.Size(*reclaimable) + " reclaimable"
	}
	item.Detail = "Docker Engine reported usage. Images and build cache can share data."
	return item
}
