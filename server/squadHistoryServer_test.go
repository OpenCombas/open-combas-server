package server

import (
	"ChromehoundsStatusServer/constants"
	"encoding/binary"
	"testing"
	"time"
)

// a fixed instant so the packed date bytes are deterministic (2026-07-10 12:34:56 UTC).
const historyTestTs int64 = 1783686896

func testHi() UserHelloMessage {
	return UserHelloMessage{
		Xuid:  [16]byte{'0', '0', '0', '9', '0', '0', '0', '0', '1', '2', '3', '4', '5', '6', '7', '8'},
		Order: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	}
}

func TestHistoryRecordJoined(t *testing.T) {
	// Use a synthetic pilot name -- never a name from logs/captures (open-source project).
	rec := historyRecord(SquadHistoryEvent{Type: historyTypeJoined, CreatedAt: historyTestTs, Name: "Pilot-01"})

	if got := binary.BigEndian.Uint16(rec[0:2]); got != historyTypeJoined {
		t.Errorf("type @0 = %d, want %d", got, historyTypeJoined)
	}
	tm := time.Unix(historyTestTs, 0).UTC()
	if got := binary.BigEndian.Uint16(rec[2:4]); got != uint16(tm.Year()) {
		t.Errorf("year @2 = %d, want %d", got, tm.Year())
	}
	if rec[4] != byte(tm.Month()) || rec[5] != byte(tm.Day()) || rec[6] != byte(tm.Hour()) || rec[7] != byte(tm.Minute()) || rec[8] != byte(tm.Second()) {
		t.Errorf("date bytes @4..8 = %v, want %d/%d %d:%d:%d", rec[4:9], tm.Month(), tm.Day(), tm.Hour(), tm.Minute(), tm.Second())
	}
	if got := nullTerm(rec[9:69]); got != "Pilot-01" {
		t.Errorf("name @9 = %q, want %q", got, "Pilot-01")
	}
}

func TestHistoryRecordInvadingIdsInBlock2(t *testing.T) {
	rec := historyRecord(SquadHistoryEvent{Type: historyTypeInvading, CreatedAt: historyTestTs, AreaID: 12, MapID: 3})
	if binary.BigEndian.Uint16(rec[0:2]) != historyTypeInvading {
		t.Fatalf("type @0 = %d, want %d", binary.BigEndian.Uint16(rec[0:2]), historyTypeInvading)
	}
	if got := nullTerm(rec[105:108]); got != "3" { // id2 (map) @+105
		t.Errorf("map id @105 = %q, want \"3\"", got)
	}
	if got := nullTerm(rec[108:134]); got != "12" { // id1 (area) @+108
		t.Errorf("area id @108 = %q, want \"12\"", got)
	}
	// Block-1 content stays empty for battle events (the map name is resolved from the ids).
	if rec[9] != 0 {
		t.Errorf("block-1 content @9 = %d, want 0 for a battle event", rec[9])
	}
}

func TestBuildHistoryResponseFramingAndTerminator(t *testing.T) {
	evs := []SquadHistoryEvent{
		{Type: historyTypeJoined, CreatedAt: historyTestTs, Name: "Pilot-01"},
		{Type: historyTypeLeft, CreatedAt: historyTestTs, Name: "Pilot-02"},
	}
	buf := buildHistoryResponse(testHi(), evs)

	if len(buf) != constants.SquadHistoryResponseSize {
		t.Fatalf("response size = %d, want %d", len(buf), constants.SquadHistoryResponseSize)
	}
	if buf[0] != 'C' || buf[1] != 'H' {
		t.Errorf("header magic = %q, want CH", buf[0:2])
	}
	if buf[constants.MinHelloMessageSize] != 0 {
		t.Errorf("status byte = %d, want 0", buf[constants.MinHelloMessageSize])
	}
	recBase := constants.MinHelloMessageSize + 1
	rec := func(i int) []byte {
		off := recBase + i*constants.SquadHistoryRecordSize
		return buf[off : off+constants.SquadHistoryRecordSize]
	}
	if binary.BigEndian.Uint16(rec(0)[0:2]) != historyTypeJoined {
		t.Errorf("slot 0 type = %d, want %d (joined)", binary.BigEndian.Uint16(rec(0)[0:2]), historyTypeJoined)
	}
	if binary.BigEndian.Uint16(rec(1)[0:2]) != historyTypeLeft {
		t.Errorf("slot 1 type = %d, want %d (left)", binary.BigEndian.Uint16(rec(1)[0:2]), historyTypeLeft)
	}
	// Slot 2 (first unused) must be a type-0 terminator so the client stops reading.
	if got := binary.BigEndian.Uint16(rec(2)[0:2]); got != historyTypeTerminator {
		t.Errorf("slot 2 type = %d, want 0 (terminator)", got)
	}
}

func TestBuildHistoryResponseEmptyIsLoneTerminator(t *testing.T) {
	buf := buildHistoryResponse(testHi(), nil)
	if buf[constants.MinHelloMessageSize] != 0 {
		t.Errorf("status = %d, want 0", buf[constants.MinHelloMessageSize])
	}
	recBase := constants.MinHelloMessageSize + 1
	if got := binary.BigEndian.Uint16(buf[recBase : recBase+2]); got != historyTypeTerminator {
		t.Errorf("first record type = %d, want 0 (empty history = lone terminator)", got)
	}
}

// nullTerm returns the bytes up to the first NUL as a string.
func nullTerm(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
