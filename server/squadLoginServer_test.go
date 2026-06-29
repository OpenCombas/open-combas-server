package server

import (
	"ChromehoundsStatusServer/constants"
	"encoding/binary"
	"testing"
)

// squadXuid is the ASCII-hex XUID seen in the create-squad captures ("000900001AC5EE91").
var squadXuid = [16]byte{'0', '0', '0', '9', '0', '0', '0', '0', '1', 'A', 'C', '5', 'E', 'E', '9', '1'}

// makeRequestPacket builds a CH request packet (32-byte header + body) like the game sends.
func makeRequestPacket(xuid [16]byte, order [8]byte, body string) []byte {
	hdr := CreateHeader(xuid, order)
	buf := make([]byte, constants.MinHelloMessageSize+len(body)+1)
	if _, err := binary.Encode(buf, binary.LittleEndian, hdr); err != nil {
		panic(err)
	}
	copy(buf[constants.MinHelloMessageSize:], body)
	return buf
}

// TestSquadLoginPopulated verifies that a login with a real team id returns a 1248-byte squad record
// whose status is 0 (valid) and whose single member is the requester, placed at the exact offsets the
// client parser (sub_823BCD88) reads.
func TestSquadLoginPopulated(t *testing.T) {
	packet := makeRequestPacket(squadXuid, [8]byte{}, "ibac,TM0001000000000001")
	hi := UserHelloMessage{Xuid: squadXuid}

	state := CreateSquadLoginState(hi, packet)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)

	body := buf[constants.MinHelloMessageSize:] // 1248-byte squad-data body
	if len(body) != 1248 {
		t.Fatalf("body size = %d, want 1248", len(body))
	}

	// TeamInfo header
	if body[0] != 0 {
		t.Errorf("status byte = %d, want 0 (valid team)", body[0])
	}
	if name := string(body[1:11]); name != "OpenCombas" {
		t.Errorf("team name = %q, want OpenCombas", name)
	}
	if body[17] != 'B' {
		t.Errorf("country code = %q, want 'B'", body[17])
	}
	if body[18] != 1 {
		t.Errorf("member count = %d, want 1", body[18])
	}

	// Member[0] at offset 288 (stride 48).
	gotXuid := int64(binary.LittleEndian.Uint64(body[288:296]))
	if gotXuid != 0x000900001AC5EE91 {
		t.Errorf("member XUID = %#x, want 0x000900001AC5EE91", gotXuid)
	}
	if uid := string(body[296:314]); uid != "US0001000000000001" {
		t.Errorf("member UserID = %q, want US0001000000000001", uid)
	}
	if name := string(body[315:319]); name != "ibac" {
		t.Errorf("member UserName = %q, want ibac", name)
	}
	if body[332] != 1 {
		t.Errorf("leader flag = %d, want 1", body[332])
	}
	if rank := body[333:336]; rank[0] != 0 || rank[1] != 0 || rank[2] != 1 {
		t.Errorf("rank bytes = % x, want 00 00 01", rank)
	}
}

// TestSquadLoginNoTeam verifies that a login with an all-zero team id (the sign-in probe before any
// squad exists) returns a non-zero status byte, which makes the client treat the record as "no team".
func TestSquadLoginNoTeam(t *testing.T) {
	packet := makeRequestPacket(squadXuid, [8]byte{}, "ibac,TM0000000000000000")
	hi := UserHelloMessage{Xuid: squadXuid}

	state := CreateSquadLoginState(hi, packet)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)

	if status := buf[constants.MinHelloMessageSize]; status == 0 {
		t.Errorf("status byte = 0, want non-zero (no team) for empty team id")
	}
}

func TestTeamIDIsEmpty(t *testing.T) {
	cases := map[string]bool{
		"TM0000000000000000": true,
		"":                   true,
		"TM":                 true,
		"TM0001000000000001": false,
	}
	for in, want := range cases {
		if got := teamIDIsEmpty(in); got != want {
			t.Errorf("teamIDIsEmpty(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestXuidToInt64(t *testing.T) {
	if got := xuidToInt64(squadXuid); got != 0x000900001AC5EE91 {
		t.Errorf("xuidToInt64 = %#x, want 0x000900001AC5EE91", got)
	}
}

// TestSquadAck verifies the lock / config-upload ack returns success ('1') with a 34-byte response.
func TestSquadAck(t *testing.T) {
	state := CreateSquadAckState(squadXuid, [8]byte{})
	buf := encodeToBuffer(state, constants.SquadAckResponseSize, t)

	if len(buf) != 34 {
		t.Fatalf("response size = %d, want 34", len(buf))
	}
	if status := buf[constants.MinHelloMessageSize]; status != '1' {
		t.Errorf("status byte = %q, want '1' (success)", status)
	}
}
