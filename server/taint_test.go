package server

import "testing"

// TestTaintFillAndDecode is a round-trip: fill a region, then decode it back to (tag, offset). The markers
// must survive the round trip so a value found in client memory names its exact wire source.
func TestTaintFillAndDecode(t *testing.T) {
	buf := make([]byte, 84) // e.g. the WorldArea unused-records region
	TaintFill(buf, TaintWorldArea)

	// Every 4-byte slot is a marker: FE, tag, off>>8, off.
	for k := 0; k < len(buf); k += 4 {
		if buf[k] != 0xFE {
			t.Fatalf("slot %d: sentinel = %#x, want 0xFE", k, buf[k])
		}
		if buf[k+1] != byte(TaintWorldArea) {
			t.Errorf("slot %d: tag = %#x, want %#x", k, buf[k+1], byte(TaintWorldArea))
		}
		off := int(buf[k+2])<<8 | int(buf[k+3])
		if off != k {
			t.Errorf("slot %d: encoded offset = %d, want %d", k, off, k)
		}
	}

	hits := TaintDecode(buf)
	if len(hits) != len(buf)/4 {
		t.Fatalf("decoded %d hits, want %d", len(hits), len(buf)/4)
	}
	for i, h := range hits {
		if h.Tag != TaintWorldArea || h.TagName != "worldarea" || h.SrcOffset != i*4 {
			t.Errorf("hit %d = %+v, want tag worldarea offset %d", i, h, i*4)
		}
	}
}

// TestTaintDecodeSwapped: a client that byte-swaps the marker dword still decodes, so we can read the
// source regardless of the deserializer's endianness.
func TestTaintDecodeSwapped(t *testing.T) {
	// {FE, tag=World(1), 0x01, 0x2C} byte-reversed = {0x2C, 0x01, 0x01, 0xFE}, offset 0x012C.
	swapped := []byte{0x2C, 0x01, 0x01, 0xFE}
	hits := TaintDecode(swapped)
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if !hits[0].Swapped || hits[0].Tag != TaintWorld || hits[0].SrcOffset != 0x012C {
		t.Errorf("swapped decode = %+v, want world offset 0x12C swapped", hits[0])
	}
}

// TestTaintDecodeIgnoresNonMarkers: real data (zeros, pointers) must not decode as false markers -- an
// unknown tag byte after a stray 0xFE is rejected.
func TestTaintDecodeIgnoresNonMarkers(t *testing.T) {
	data := []byte{0x00, 0x00, 0x00, 0x00, 0xFE, 0x99, 0x00, 0x00} // 0x99 is not a defined tag
	if hits := TaintDecode(data); len(hits) != 0 {
		t.Errorf("decoded %d hits from non-marker data, want 0: %+v", len(hits), hits)
	}
}
