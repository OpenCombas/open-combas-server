package server

import "testing"

func TestSeasonKey(t *testing.T) {
	for n, want := range map[int]string{1: "0001", 14: "0014", 114: "0114", 2026: "2026"} {
		if got := SeasonKey(n); got != want {
			t.Errorf("SeasonKey(%d) = %q, want %q", n, got, want)
		}
	}
}

// ApplySeasonNumber must move both the exported number and the stats bucket key together.
func TestApplySeasonNumber(t *testing.T) {
	orig := SeasonNumber()
	defer ApplySeasonNumber(orig)
	ApplySeasonNumber(21)
	if SeasonNumber() != 21 || currentSeason != "0021" {
		t.Errorf("after ApplySeasonNumber(21): SeasonNumber=%d currentSeason=%q, want 21/0021", SeasonNumber(), currentSeason)
	}
	ApplySeasonNumber(0) // non-positive falls back to default
	if SeasonNumber() != defaultSeasonNumber {
		t.Errorf("ApplySeasonNumber(0) = %d, want default %d", SeasonNumber(), defaultSeasonNumber)
	}
}
