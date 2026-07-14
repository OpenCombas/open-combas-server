package server

import (
	"testing"
	"time"
)

func TestNewsEntryFromEvent(t *testing.T) {
	ts := time.Date(2026, 7, 9, 12, 34, 0, 0, time.UTC).Unix()
	e := newsEntryFromEvent(EventRecord{CreatedAt: ts, TemplateID: 35, Slot1: "North Village Ruin", Slot2: "AlphaSquad"})
	if e.TemplateID != 35 {
		t.Errorf("template = %d, want 35", e.TemplateID)
	}
	// Full date sourced from CreatedAt: Code = [month, day, hour, minute], EntityID = 2-digit year.
	// 2026-07-09 12:34 -> header "07/09/26 12:34".
	if e.EntityID != 26 {
		t.Errorf("EntityID = %d, want 26 (2-digit year of 2026)", e.EntityID)
	}
	if e.Code != [5]byte{7, 9, 12, 34, 0} {
		t.Errorf("code = %v, want [7 9 12 34 0]", e.Code)
	}
	// C66 = two 33-byte slots: slot1 @ [0..32], slot2 @ [33..65], each NUL-terminated within its slot.
	if got := string(e.Text[0:18]); got != "North Village Ruin" {
		t.Errorf("slot1 = %q, want North Village Ruin", got)
	}
	if e.Text[32] != 0 {
		t.Error("slot1 must be NUL-terminated at [32] so it can't bleed into slot2")
	}
	if got := string(e.Text[33:43]); got != "AlphaSquad" {
		t.Errorf("slot2 = %q, want AlphaSquad", got)
	}
}

func TestNewsFromEvents(t *testing.T) {
	// 12 events -> block 0 gets the first 10, block 1 the remaining 2, order preserved (caller passes
	// newest-first).
	evs := make([]EventRecord, 12)
	for i := range evs {
		evs[i] = EventRecord{CreatedAt: int64(1000 - i), TemplateID: int32(i + 1)}
	}
	s := newsFromEvents(squadXuid, [8]byte{}, evs)
	if s.Blocks[0].Status != 0 || s.Blocks[0].RecordNum != 10 {
		t.Errorf("block0 status/count = %d/%d, want 0/10", s.Blocks[0].Status, s.Blocks[0].RecordNum)
	}
	if s.Blocks[1].RecordNum != 2 {
		t.Errorf("block1 count = %d, want 2", s.Blocks[1].RecordNum)
	}
	if s.Blocks[0].Entries[0].TemplateID != 1 || s.Blocks[1].Entries[0].TemplateID != 11 {
		t.Errorf("first entries = %d / %d, want 1 / 11", s.Blocks[0].Entries[0].TemplateID, s.Blocks[1].Entries[0].TemplateID)
	}
}

func TestNewsFromEventsEmpty(t *testing.T) {
	// No events -> both blocks accepted (status 0) but zero count, so the client shows an empty board
	// (no fabricated "nation dissolved" stories).
	s := newsFromEvents(squadXuid, [8]byte{}, nil)
	if s.Blocks[0].RecordNum != 0 || s.Blocks[1].RecordNum != 0 {
		t.Errorf("empty news should have zero counts, got %d/%d", s.Blocks[0].RecordNum, s.Blocks[1].RecordNum)
	}
}

func TestBriefingNeverEmpty(t *testing.T) {
	ev := briefingEvent(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC).Unix())
	if ev.TemplateID != initEventRow {
		t.Errorf("briefing row = %d, want %d (param row 75 = War Breaks Out)", ev.TemplateID, initEventRow)
	}
	// The briefing must render as a non-zero-count board (a zero count is rejected as "communication failed").
	s := newsFromEvents(squadXuid, [8]byte{}, []EventRecord{ev})
	if s.Blocks[0].Status != 0 || s.Blocks[0].RecordNum != 1 {
		t.Errorf("briefing board = status %d count %d, want status 0 count 1", s.Blocks[0].Status, s.Blocks[0].RecordNum)
	}
}

func TestParseNewsRequest(t *testing.T) {
	// Request body is "<gamertag>,<nation>,<category>": category 1 = political, 2 = war.
	if n, c := parseNewsRequest(makeRequestPacket(squadXuid, [8]byte{}, "player1,A,1")); n != 'A' || c != newsCategoryPolitical {
		t.Errorf("nation/category = %q/%d, want 'A'/1", n, c)
	}
	if n, c := parseNewsRequest(makeRequestPacket(squadXuid, [8]byte{}, "player1,B,2")); n != 'B' || c != newsCategoryWar {
		t.Errorf("nation/category = %q/%d, want 'B'/2", n, c)
	}
	if n, c := parseNewsRequest(makeRequestPacket(squadXuid, [8]byte{}, "player1")); n != 0 || c != newsCategoryPolitical {
		t.Errorf("missing fields -> %q/%d, want 0/default 1", n, c)
	}
}

func TestFilterNewsByCategory(t *testing.T) {
	evs := []EventRecord{
		{TemplateID: 1},  // dissolution -> war
		{TemplateID: 35}, // battlefield capture -> war
		{TemplateID: 75}, // "War Breaks Out" briefing -> excluded from both (fallback only)
		{TemplateID: 41}, // president takes office -> political
		{TemplateID: 69}, // massive donation -> political
	}
	war := filterNewsByCategory(evs, 'A', newsCategoryWar)
	if len(war) != 2 {
		t.Errorf("war news = %d events, want 2 (rows 1/35; briefing 75 excluded)", len(war))
	}
	for _, e := range war {
		if !newsRowIsWar(e.TemplateID) {
			t.Errorf("row %d classed as war but isn't", e.TemplateID)
		}
	}
	poli := filterNewsByCategory(evs, 'A', newsCategoryPolitical)
	if len(poli) != 2 {
		t.Errorf("political news = %d events, want 2 (rows 41/69)", len(poli))
	}
	// The briefing must not appear in either domain feed.
	for _, e := range append(war, poli...) {
		if e.TemplateID == 75 {
			t.Error("briefing (row 75) leaked into a domain feed; it should be fallback-only")
		}
	}
}

func TestCaptureRows(t *testing.T) {
	if !isHQ(1) || !isHQ(2) || !isHQ(3) || isHQ(4) || isHQ(22) {
		t.Error("isHQ wrong: areas 1/2/3 are capitals, 4+ are not")
	}
	for loser, want := range map[byte]int32{'B': 29, 'C': 30, 'A': 31} {
		if got, ok := regionCaptureRow(loser); !ok || got != want {
			t.Errorf("regionCaptureRow(%c) = %d,%v want %d", loser, got, ok, want)
		}
	}
	for loser, want := range map[byte]int32{'B': 35, 'C': 36, 'A': 37} {
		if got, ok := battlefieldCaptureRow(loser); !ok || got != want {
			t.Errorf("battlefieldCaptureRow(%c) = %d,%v want %d", loser, got, ok, want)
		}
	}
	if _, ok := regionCaptureRow('Z'); ok {
		t.Error("regionCaptureRow('Z') should be false")
	}
}

func TestDissolutionRow(t *testing.T) {
	// One param row per loser/winner pair (WorldSituationInfoNewsParam.bin).
	cases := map[[2]byte]int32{
		{'A', 'B'}: 3, {'A', 'C'}: 5, // Xeres/Tarakia -> Morskoj / Sal Kar
		{'B', 'A'}: 1, {'B', 'C'}: 6, // Ostrov/Morskoj -> Tarakia / Sal Kar
		{'C', 'A'}: 2, {'C', 'B'}: 4, // Qara/Sal Kar -> Tarakia / Morskoj
	}
	for pair, want := range cases {
		if got, ok := dissolutionRow(pair[0], pair[1]); !ok || got != want {
			t.Errorf("dissolutionRow(%c,%c) = %d,%v want %d,true", pair[0], pair[1], got, ok, want)
		}
	}
	if _, ok := dissolutionRow('A', 'A'); ok {
		t.Error("dissolutionRow with loser==winner should be false")
	}
	if _, ok := dissolutionRow('Z', 'A'); ok {
		t.Error("dissolutionRow('Z','A') should be false")
	}
}
