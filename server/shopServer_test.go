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
	// Block 0 name and block 1 name at their 1012-byte stride.
	if !bytes.HasPrefix(body[0:20], []byte("BLOCK0")) {
		t.Errorf("block0 name = %q, want BLOCK0...", body[0:20])
	}
	if !bytes.HasPrefix(body[1012:1012+20], []byte("BLOCK1")) {
		t.Errorf("block1 name = %q, want BLOCK1...", body[1012:1012+20])
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
