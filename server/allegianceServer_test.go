package server

import (
	"ChromehoundsStatusServer/constants"
	"testing"
)

// buildAllegiancePacket frames a header + "<account>,<teamId>,<nation>" body, as the client sends to 1221.
func buildAllegiancePacket(account, teamID string, nation byte) []byte {
	body := append([]byte(account+","+teamID+","), nation)
	body = append(body, 0) // the client NUL-terminates the printf body (see the captured 65-byte datagram)
	pkt := make([]byte, constants.MinHelloMessageSize+len(body))
	copy(pkt, ChromeHoundsHeader[:])
	copy(pkt[constants.MinHelloMessageSize:], body)
	return pkt
}

func TestParseAllegiance(t *testing.T) {
	// The exact body captured from the retail client (season-start switch to Sal Kar).
	acc, team, nation, ok := parseAllegiance(buildAllegiancePacket("ibacinstall", "TM0001000000000032", 'C'))
	if !ok {
		t.Fatal("parse failed on a valid body")
	}
	if acc != "ibacinstall" || team != "TM0001000000000032" || nation != 'C' {
		t.Errorf("parsed (%q,%q,%q), want (ibacinstall, TM0001000000000032, C)", acc, team, string(nation))
	}

	// Too few fields (missing nation) must not parse.
	if _, _, _, ok := parseAllegiance(buildShortAllegiance("acc,TM123")); ok {
		t.Error("parsed a body with no nation field")
	}
	// Header-only packet must not parse.
	if _, _, _, ok := parseAllegiance(make([]byte, constants.MinHelloMessageSize)); ok {
		t.Error("parsed a header-only packet")
	}
}

func buildShortAllegiance(body string) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+len(body))
	copy(pkt, ChromeHoundsHeader[:])
	copy(pkt[constants.MinHelloMessageSize:], body)
	return pkt
}

// TestAllegianceStatusBytes pins the status codes to what parser sub_823BE000 decodes: '1' Complete,
// '2' same-state, anything else "Unknown Error". A drift here changes the popup the player sees.
func TestAllegianceStatusBytes(t *testing.T) {
	if allegianceComplete != '1' || allegianceSameState != '2' {
		t.Fatalf("status bytes drifted: complete=%q same=%q (want '1','2')", string(allegianceComplete), string(allegianceSameState))
	}
	if allegianceError == '1' || allegianceError == '2' {
		t.Fatalf("allegianceError %q collides with a success code", string(allegianceError))
	}
	// The ack the client reads: status must sit at the first body byte.
	resp := squadAckState([16]byte{}, [8]byte{}, allegianceSameState)
	if resp.Status != '2' {
		t.Errorf("ack Status = %q, want '2'", string(resp.Status))
	}
}
