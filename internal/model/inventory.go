package model

// InventoryEntry is a file or directory in a detailed capture. Bytes are
// attributed just once, even when several paths are hard links to a file.
// Parent is empty for the top-level entries of the selected scan.
type InventoryEntry struct {
	Path      string
	Parent    string
	Kind      string
	Bytes     int64
	Length    int64
	Modified  int64
	Files     int64
	Directory bool
	Offline   bool
	Shared    bool
}

// DuplicateCopy represents one physical file and the paths pointing to it.
type DuplicateCopy struct {
	Paths []string
	Bytes int64
	Links uint32
}

func (c DuplicateCopy) Reclaimable() int64 {
	if c.Links == 0 || uint64(c.Links) != uint64(len(c.Paths)) {
		return 0
	}
	return c.Bytes
}

type DuplicateGroup struct {
	Length int64
	Copies []DuplicateCopy
}

func (g DuplicateGroup) Reclaimable(keep int) int64 {
	var bytes int64
	for i, copy := range g.Copies {
		if i != keep {
			bytes += copy.Reclaimable()
		}
	}
	return bytes
}

func (g DuplicateGroup) SuggestedKeep() int {
	keep := 0
	for i := 1; i < len(g.Copies); i++ {
		if g.Copies[i].Reclaimable() < g.Copies[keep].Reclaimable() {
			keep = i
		}
	}
	return keep
}

type DuplicateResult struct {
	Groups  []DuplicateGroup
	Checked int64
	Skipped int64
	Offline int64
}
