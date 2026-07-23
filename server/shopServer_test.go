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
	resp := buildMarkerShop([16]byte{}, [8]byte{})
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
	// rejects the whole reply ("failure to parse"). This is the bug the layout fix addresses.
	if body[0] != 0 || body[1012] != 0 {
		t.Fatalf("block status bytes must be 0: block0[0]=%d block1[0]=%d", body[0], body[1012])
	}
	// Name string is at +1 (after the status byte), 19 bytes.
	if !bytes.HasPrefix(body[1:20], []byte("BLK0")) {
		t.Errorf("block0 name = %q, want BLK0...", body[1:20])
	}
	if !bytes.HasPrefix(body[1013:1013+19], []byte("BLK1")) {
		t.Errorf("block1 name = %q, want BLK1...", body[1013:1013+19])
	}
	// Entry count is the BYTE at +50 (not the int16 at +48).
	if body[50] != byte(len(validPartCodes)) {
		t.Errorf("block0 entry-count byte at +50 = %d, want %d", body[50], len(validPartCodes))
	}
	// Block 0, entry 0: Price 100000 (LE int32) at header(52)+0; Code = a real part code at +8 so the row
	// renders (marker codes are dropped by the client's part-DB check).
	e0 := body[52:]
	if got := int32(binary.LittleEndian.Uint32(e0[0:4])); got != 100000 {
		t.Errorf("block0 item0 price = %d, want 100000", got)
	}
	if string(e0[8:12]) != validPartCodes[0] {
		t.Errorf("block0 item0 code = %q, want %q (a real part code)", e0[8:12], validPartCodes[0])
	}
	// Block 1, entry 5: Price 200005; Code "B005".
	e := body[1012+52+5*12:]
	if got := int32(binary.LittleEndian.Uint32(e[0:4])); got != 200005 {
		t.Errorf("block1 item5 price = %d, want 200005", got)
	}
	if string(e[8:12]) != "B005" {
		t.Errorf("block1 item5 code = %q, want B005", e[8:12])
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
