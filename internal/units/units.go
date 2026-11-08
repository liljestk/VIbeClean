// Package units formats byte counts for people.
package units

import "fmt"

// Binary size units.
const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
	TiB int64 = 1 << 40
)

var scale = []struct {
	size int64
	name string
}{{TiB, "TiB"}, {GiB, "GiB"}, {MiB, "MiB"}, {KiB, "KiB"}}

// Magnitude returns the absolute value of a byte count.
func Magnitude(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// Size formats a byte count. A negative count means the size is unknown.
func Size(value int64) string {
	if value < 0 {
		return "—"
	}
	for _, unit := range scale {
		if value >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(value)/float64(unit.size), unit.name)
		}
	}
	return fmt.Sprintf("%d B", value)
}

// Delta formats a signed change. A bounded change is a lower limit, so it is
// shown as "≥" with an arrow for its direction.
func Delta(value int64, bounded bool) string {
	switch {
	case value == 0:
		return "0 B"
	case bounded && value < 0:
		return "≥ " + Size(Magnitude(value)) + " ↓"
	case bounded:
		return "≥ " + Size(value) + " ↑"
	case value < 0:
		return "−" + Size(Magnitude(value))
	default:
		return "+" + Size(value)
	}
}
