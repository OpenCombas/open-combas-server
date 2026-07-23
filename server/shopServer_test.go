package server

import (
	"ChromehoundsStatusServer/constants"
	"bytes"
	"encoding/binary"
	"testing"
)

// TestShopResponseWireLayout pins the sizes the client parser sub_823BDDE8 expects: 2 blocks of 1012 bytes
// (52-byte header + 80x12), total reply 2056 bytes, little-endian. A drift here means the parser walks off
// the record boundary and every field after it is garbage.
func TestShopResponseWireLayout(t *testing.T) {
	if shopBlockSize != 1012 {
		t.Fatalf("shopBlockSize = %d, want 1012", shopBlockSize)
	}
	if shopResponseSize != 2056 {
		t.Fatalf("shopResponseSize = %d, want 2056", shopResponseSize)
	}
	resp := buildShop([16]byte{}, [8]byte{})
	buf := make([]byte, shopResponseSize)
	n, err := binary.Encode(buf, binary.LittleEndian, resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if n != shopResponseSize {
		t.Fatalf("encoded %d bytes, want %d (struct has hidden padding?)", n, shopResponseSize)
	}

	body := buf[constants.MinHelloMessageSize:]
	// The critical invariant: the first byte of BOTH blocks is the status byte and MUST be 0, or the client
	// rejects the whole reply ("failure to parse"). This is the bug the layout fix addressed.
	if body[0] != 0 || body[1012] != 0 {
		t.Fatalf("block status bytes must be 0: block0[0]=%d block1[0]=%d", body[0], body[1012])
	}
	// Name string is at +1 (after the status byte), 19 bytes.
	if !bytes.HasPrefix(body[1:20], []byte("COMBAS SHOP 0")) {
		t.Errorf("block0 name = %q, want COMBAS SHOP 0...", body[1:20])
	}
	half := (len(shopCatalogue) + 1) / 2
	// Entry count is the BYTE at +50 (not the int16 at +48).
	if int(body[50]) != half {
		t.Errorf("block0 entry-count byte at +50 = %d, want %d", body[50], half)
	}
	// The 5 featured "New Parts" are the dwords at block+24 (Vals[1..5]); Vals[1] must be the first real
	// descriptor of block 0's slice, LE-encoded so the client reads back 0xMMSSIIII unchanged.
	if got := binary.LittleEndian.Uint32(body[24:28]); got != shopCatalogue[0] {
		t.Errorf("block0 featured[0] at +24 = 0x%08x, want 0x%08x", got, shopCatalogue[0])
	}
	// Block 0, entry 0: Price 1000 (LE int32) at header(52)+0; Desc = the packed descriptor at +8. The whole
	// point of the fix: this is a uint32 the client resolves against its part DB, not a 4-char string.
	e0 := body[52:]
	if got := int32(binary.LittleEndian.Uint32(e0[0:4])); got != 1000 {
		t.Errorf("block0 item0 price = %d, want 1000", got)
	}
	if got := binary.LittleEndian.Uint32(e0[8:12]); got != shopCatalogue[0] {
		t.Errorf("block0 item0 desc = 0x%08x, want 0x%08x", got, shopCatalogue[0])
	}
	// Descriptor byte order sanity: major is the HIGH byte (sub_82294F28 reads HIBYTE), so on the LE wire the
	// major byte lands LAST of the four. shopCatalogue[0] = 0x01010001 -> wire 01 00 01 01.
	if e0[8] != 0x01 || e0[9] != 0x00 || e0[10] != 0x01 || e0[11] != 0x01 {
		t.Errorf("block0 item0 desc wire bytes = % x, want 01 00 01 01", e0[8:12])
	}
	// Block 1 gets the second half of the catalogue; entry 0's descriptor is shopCatalogue[half].
	e1 := body[1012+52:]
	if got := binary.LittleEndian.Uint32(e1[8:12]); got != shopCatalogue[half] {
		t.Errorf("block1 item0 desc = 0x%08x, want 0x%08x", got, shopCatalogue[half])
	}
}

// TestLotInfoResponseSize pins the lot-info reply the parser sub_823BDED8 expects: 40 entries of 24 bytes
// starting at body+4, total reply 996 bytes, and status byte 0.
func TestLotInfoResponseSize(t *testing.T) {
	if lotInfoResponseSize != 996 {
		t.Fatalf("lotInfoResponseSize = %d, want 996", lotInfoResponseSize)
	}
	resp := buildMarkerLotInfo([16]byte{}, [8]byte{})
	buf := make([]byte, lotInfoResponseSize)
	n, err := binary.Encode(buf, binary.LittleEndian, resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if n != lotInfoResponseSize {
		t.Fatalf("encoded %d bytes, want %d (hidden struct padding?)", n, lotInfoResponseSize)
	}
	body := buf[constants.MinHelloMessageSize:]
	if body[0] != 0 {
		t.Errorf("lot-info status byte must be 0, got %d", body[0])
	}
	// First entry Value 200000 at body+4 (LE int32).
	if got := int32(binary.LittleEndian.Uint32(body[4:8])); got != 200000 {
		t.Errorf("lot item0 value = %d, want 200000", got)
	}
}
