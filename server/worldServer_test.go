package server

import "testing"

// TestPresidentIDsAreInOwnNationBlock pins the per-nation president key ranges taken from
// menu/WorldSituationInfoNewsPresidentParam.bin. PresidentID is a KEY into that table (client lookup
// sub_822A1D20), and the table is three contiguous blocks of 13/12/10 -- so serving 1/2/3 for the three
// nations put every one of them inside Tarakia's block and displayed three Tarakian presidents.
func TestPresidentIDsAreInOwnNationBlock(t *testing.T) {
	for _, n := range defaultNations() {
		first, last, ok := presidentRangeFor(n.CountryCode)
		if !ok {
			t.Fatalf("no president range for country code %q", n.CountryCode)
		}
		if n.PresidentID < first || n.PresidentID > last {
			t.Errorf("nation %q president %d outside its block %d..%d",
				n.CountryCode, n.PresidentID, first, last)
		}
	}
	// The blocks must not overlap, or an id would be ambiguous between nations.
	seen := map[byte]byte{}
	for _, code := range []byte{'A', 'B', 'C'} {
		first, last, _ := presidentRangeFor(code)
		for id := first; id <= last; id++ {
			if other, dup := seen[id]; dup {
				t.Errorf("president id %d claimed by both %q and %q", id, other, code)
			}
			seen[id] = code
		}
	}
	if len(seen) != 35 {
		t.Errorf("president blocks cover %d ids, want 35 (the table's entry count)", len(seen))
	}
}

// TestClampPresidentIDRepairsLegacyDocs covers the persisted path: nation docs seeded before the block
// mapping was known hold 1/2/3, which renders as three Tarakian leaders rather than failing visibly.
func TestClampPresidentIDRepairsLegacyDocs(t *testing.T) {
	cases := []struct {
		code byte
		in   byte
		want byte
	}{
		{'A', 1, 1},   // already valid, untouched
		{'A', 13, 13}, // upper bound of its block
		{'B', 2, 14},  // legacy value from Tarakia's block -> Morskoj's first
		{'B', 25, 25},
		{'C', 3, 26},  // legacy -> Sal Kar's first
		{'C', 36, 26}, // past the end of the whole table
		{'?', 2, 2},   // unknown nation passes through
	}
	for _, c := range cases {
		if got := clampPresidentID(c.code, c.in); got != c.want {
			t.Errorf("clampPresidentID(%q, %d) = %d, want %d", c.code, c.in, got, c.want)
		}
	}
}
