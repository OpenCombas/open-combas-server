package server

import (
	"ChromehoundsStatusServer/constants"
	"testing"
)

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

// TestNationUnknown57ReachesTheWire proves offset 57 round-trips from a stored nation doc to the wire
// byte, so an in-game test of that field measures the CLIENT's behaviour rather than our plumbing.
//
// Byte 57 sits inside the nation record's trailing "C3" schema group (56 PresidentID, 57 ?, 58 DeadFlag),
// so the client genuinely deserializes it -- it is not padding. It is the only per-nation byte we have
// never identified, and the unidentified weapon is a per-nation state, which is why it is a candidate for
// the map-state selector. Nothing here claims it IS that; this only guarantees a set value arrives.
func TestNationUnknown57ReachesTheWire(t *testing.T) {
	nations := defaultNations()
	nations[2].Unknown57 = 1 // Sal Kar

	state := newWorldState([16]byte{}, [8]byte{}, nations)
	buf := encodeToBuffer(state, constants.WorldResponseSize, t)
	body := buf[constants.MinHelloMessageSize:]

	// body: WorldHeader(28) + 3 nation records of 60 bytes.
	const nationBase, nationStride = 28, 60
	for i, want := range []byte{0, 0, 1} {
		off := nationBase + i*nationStride + 57
		if got := body[off]; got != want {
			t.Errorf("nation %d byte57 at body[%d] = %d, want %d", i, off, got, want)
		}
	}
	// Sanity: the neighbouring bytes in the same C3 group must be undisturbed.
	salKar := nationBase + 2*nationStride
	if got := body[salKar+56]; got != presidentSalKarFirst {
		t.Errorf("Sal Kar PresidentID = %d, want %d (byte57 must not disturb it)", got, presidentSalKarFirst)
	}
	if got := body[salKar+58]; got != 0 {
		t.Errorf("Sal Kar DeadFlag = %d, want 0", got)
	}
}

// TestFillWeaponFuzz pins the provenance pattern: each Tail byte is 0x80|(k&0x7F), never zero, and the low
// 7 bits decode to the Tail offset (== World body offset 208+k) so a run seen in the client's weapon object
// names its source. Tail[0] corresponds to World body +208, the "84 B parsed elsewhere" region.
func TestFillWeaponFuzz(t *testing.T) {
	var tail [332]byte
	fillWeaponFuzz(&tail)
	for _, k := range []int{0, 1, 27, 83, 128} {
		want := byte(0x80 | (k & 0x7F))
		if tail[k] != want {
			t.Errorf("tail[%d] = %#x, want %#x", k, tail[k], want)
		}
		if tail[k] == 0 {
			t.Errorf("tail[%d] is zero; markers must be non-zero to be distinguishable", k)
		}
	}
	// The 84-byte weapon region (k=0..83) must not wrap (all <= 0xD3), so offsets stay unique there.
	if tail[83] != 0xD3 {
		t.Errorf("tail[83] = %#x, want 0xD3 (no wrap within the 84-byte region)", tail[83])
	}
}
