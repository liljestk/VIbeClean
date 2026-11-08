package units

import "testing"

func TestSizeAndDelta(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{Size(-1), "—"},
		{Size(512), "512 B"},
		{Size(3 * MiB / 2), "1.5 MiB"},
		{Size(2 * TiB), "2.0 TiB"},
		{Delta(0, false), "0 B"},
		{Delta(GiB, false), "+1.0 GiB"},
		{Delta(-GiB, false), "−1.0 GiB"},
		{Delta(GiB, true), "≥ 1.0 GiB ↑"},
		{Delta(-GiB, true), "≥ 1.0 GiB ↓"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	if Magnitude(-5) != 5 || Magnitude(5) != 5 {
		t.Fatal("magnitude should drop the sign")
	}
}
