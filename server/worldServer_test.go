package server

import (
	"ChromehoundsStatusServer/config"
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

// TestApplyWeaponRecords pins that a nation's WeaponRecordHex lands at the World Tail offset the taint
// proved: Sal Kar (C) at Tail[56:84] (= World body 264, weaponObj+260 slot 2). This is the emission the
// value-fuzz drives -- get the offset wrong and the fuzz maps the wrong nation.
func TestApplyWeaponRecords(t *testing.T) {
	s := &worldServer{messageServer: &messageServer{serverConfig: &config.ServerConfig{Label: "TEST"}}}
	recs := []NationRecord{
		{CountryCode: "A", WeaponRecordHex: "aa"},
		{CountryCode: "C", WeaponRecordHex: "000102030405060708090a0b0c0d0e0f101112131415161718191a1b"}, // 28 bytes 00..1b
		{CountryCode: "B"}, // empty -> stays zero
	}
	var tail [332]byte
	s.applyWeaponRecords(&tail, recs)

	if tail[0] != 0xAA {
		t.Errorf("nation A slot: tail[0] = %#x, want 0xAA", tail[0])
	}
	for k := 28; k < 56; k++ {
		if tail[k] != 0 {
			t.Errorf("nation B slot (empty) should be zero, tail[%d] = %#x", k, tail[k])
		}
	}
	// Sal Kar's 28-byte ramp lands at Tail[56..83].
	for k := 0; k < 28; k++ {
		if got := tail[56+k]; got != byte(k) {
			t.Errorf("Sal Kar record byte %d at tail[%d] = %#x, want %#x", k, 56+k, got, k)
		}
	}
}

// TestApplyWeaponRecordsTruncatesAndSkips: >28 bytes is truncated to the record width; bad hex is skipped
// (leaving zeros) rather than dropping the reply.
func TestApplyWeaponRecordsTruncatesAndSkips(t *testing.T) {
	s := &worldServer{messageServer: &messageServer{serverConfig: &config.ServerConfig{Label: "TEST"}}}
	long := ""
	for i := 0; i < 40; i++ {
		long += "ff"
	}
	recs := []NationRecord{
		{CountryCode: "A", WeaponRecordHex: long},   // 40 bytes -> truncate to 28
		{CountryCode: "B", WeaponRecordHex: "zzzz"}, // invalid hex -> skip
	}
	var tail [332]byte
	s.applyWeaponRecords(&tail, recs)
	if tail[27] != 0xFF || tail[28] != 0x00 {
		t.Errorf("truncation: tail[27]=%#x tail[28]=%#x, want ff then 00 (28-byte cap)", tail[27], tail[28])
	}
	for k := 28; k < 56; k++ {
		if tail[k] != 0 {
			t.Fatalf("bad-hex nation B should stay zero, tail[%d]=%#x", k, tail[k])
		}
	}
}
