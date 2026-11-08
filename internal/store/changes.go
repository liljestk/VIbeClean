package store

import (
	"context"
	"slices"
	"strings"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

// matcher turns version pairs into ranked changes and counts the pairs it
// omits because the two captures did not measure them the same way.
type matcher struct {
	before, after model.Coverage
	changedGaps   map[string]bool
	sameVolume    map[string]bool
	changes       []model.Change
	omitted       int
}

func newMatcher(before, after model.Coverage) *matcher {
	return &matcher{
		before:      before,
		after:       after,
		changedGaps: changedGaps(before.Gaps, after.Gaps),
		sameVolume:  sameVolumes(before.Drives, after.Drives),
	}
}

// changedGaps returns locations that were unreadable in only one capture.
func changedGaps(before, after []string) map[string]bool {
	in := func(paths []string) map[string]bool {
		set := make(map[string]bool, len(paths))
		for _, path := range paths {
			set[model.NormalizedKey(path)] = true
		}
		return set
	}
	earlier, later := in(before), in(after)
	changed := make(map[string]bool)
	for gap := range earlier {
		if !later[gap] {
			changed[gap] = true
		}
	}
	for gap := range later {
		if !earlier[gap] {
			changed[gap] = true
		}
	}
	return changed
}

// sameVolumes returns the roots that hold the same volume in both captures.
// Quick review spans every fixed drive, so an added, removed or replaced
// drive must not be reported as growth or freed space. It returns nil when
// either capture has no drive readings, such as a Docker capture.
func sameVolumes(before, after []model.DriveState) map[string]bool {
	if len(before) == 0 || len(after) == 0 {
		return nil
	}
	same := make(map[string]bool)
	for _, old := range before {
		for _, now := range after {
			if old.Root == now.Root && old.ID == now.ID {
				same[now.Root] = true
			}
		}
	}
	return same
}

func (m *matcher) driveChanged(item *model.Record) bool {
	return m.sameVolume != nil && item != nil && item.Path != "" && !m.sameVolume[model.VolumeRoot(item.Path)]
}

func (m *matcher) gapChanged(item *model.Record) bool {
	return item != nil && overlapsGap(item.Path, m.changedGaps)
}

func (m *matcher) incomparable(series string, old, fresh *model.Record) bool {
	return (series == model.Candidates && m.before.Kind != m.after.Kind) ||
		(old != nil && old.Partial) || (fresh != nil && fresh.Partial) ||
		m.driveChanged(old) || m.driveChanged(fresh) || m.gapChanged(old) || m.gapChanged(fresh)
}

func (m *matcher) add(key model.RowKey, old, fresh *model.Record) {
	if key.Series == model.Subfolders && (!m.before.Subfolders || !m.after.Subfolders) {
		return
	}
	var a, b int64
	if old != nil {
		a = old.Bytes
	}
	if fresh != nil {
		b = fresh.Bytes
	}
	if a < 0 || b < 0 || a == b {
		return
	}
	if m.incomparable(key.Series, old, fresh) {
		m.omitted++
		return
	}
	delta, bounded := b-a, false
	// Large files and subfolders are tracked from the floor, so an absent side
	// may hold up to the floor: report the smallest change that fits.
	if (key.Series == model.LargeFiles || key.Series == model.Subfolders) && (old == nil || fresh == nil) {
		bounded = true
		if old == nil {
			delta = max(0, b-model.FileFloor+1)
		} else {
			delta = -max(0, a-model.FileFloor+1)
		}
	}
	if delta != 0 {
		m.changes = append(m.changes, model.Change{Series: key.Series, Before: old, After: fresh, Bytes: delta, Bounded: bounded})
	}
}

// calculateChanges pairs versions by key first, then pairs a removed item
// with an added item at the same location so replacements show as changes.
func calculateChanges(ctx context.Context, older, newer map[model.RowKey]model.Record, before, after model.Coverage) ([]model.Change, int, error) {
	m := newMatcher(before, after)
	byLocation := make(map[model.RowKey]model.RowKey)
	for key, old := range older {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if fresh, ok := newer[key]; ok {
			m.add(key, &old, &fresh)
		} else if location := comparisonLocation(old); location != "" {
			byLocation[model.RowKey{Series: key.Series, Key: location}] = key
		}
	}
	matched := make(map[model.RowKey]bool)
	for key, fresh := range newer {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if _, ok := older[key]; ok {
			continue
		}
		location := model.RowKey{Series: key.Series, Key: comparisonLocation(fresh)}
		if oldKey, ok := byLocation[location]; ok {
			old := older[oldKey]
			m.add(key, &old, &fresh)
			matched[oldKey] = true
			delete(byLocation, location)
		} else {
			m.add(key, nil, &fresh)
		}
	}
	for key, old := range older {
		if _, ok := newer[key]; !ok && !matched[key] {
			m.add(key, &old, nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	slices.SortFunc(m.changes, compareChanges)
	return m.changes, m.omitted, nil
}

// compareChanges ranks the largest change first, then orders by name and key.
func compareChanges(a, b model.Change) int {
	if x, y := units.Magnitude(a.Bytes), units.Magnitude(b.Bytes); x != y {
		if x > y {
			return -1
		}
		return 1
	}
	x, y := a.Item(), b.Item()
	if order := strings.Compare(x.Name, y.Name); order != 0 {
		return order
	}
	if order := strings.Compare(x.Series, y.Series); order != 0 {
		return order
	}
	return strings.Compare(x.Key, y.Key)
}

func comparisonLocation(item model.Record) string {
	if item.Path != "" {
		return model.NormalizedKey(item.Path)
	}
	if item.Series == model.Containers {
		return strings.ToLower(item.Name)
	}
	return ""
}

func overlapsGap(path string, gaps map[string]bool) bool {
	if path == "" || len(gaps) == 0 {
		return false
	}
	path = model.NormalizedKey(path)
	for gap := range gaps {
		if path == gap || strings.HasPrefix(path, gap+`\`) || strings.HasPrefix(gap, path+`\`) {
			return true
		}
	}
	return false
}
