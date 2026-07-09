package server

import (
	"ChromehoundsStatusServer/constants"
	"testing"
)

func TestParseDonation(t *testing.T) {
	// Matches the real world-screen packet body "<gamertag>,<nation>" (e.g. "ibac600,A").
	pkt := makeRequestPacket(squadXuid, [8]byte{}, "ibac600,A")
	gt, nation, ok := parseDonation(pkt)
	if !ok {
		t.Fatal("parse failed")
	}
	if gt != "ibac600" || nation != 'A' {
		t.Errorf("parse = (%q,%q), want (\"ibac600\",'A')", gt, string(nation))
	}
}

func TestParseDonationMalformed(t *testing.T) {
	// No nation field -> not ok (handler falls back to a lenient '1' ack).
	pkt := makeRequestPacket(squadXuid, [8]byte{}, "ibac600")
	if _, _, ok := parseDonation(pkt); ok {
		t.Error("expected parse to fail on missing nation field")
	}
}

func TestDonationAckSize(t *testing.T) {
	// The parser sub_823BDFB0 reads body[0]; '1' = "Complete". Response is the 34-byte ack record.
	state := squadAckState(squadXuid, [8]byte{}, '1')
	buf := encodeToBuffer(state, constants.SquadAckResponseSize, t)
	if len(buf) != 34 || buf[constants.MinHelloMessageSize] != '1' {
		t.Errorf("donation ack size/status wrong: len=%d status=%q", len(buf), buf[constants.MinHelloMessageSize])
	}
}
