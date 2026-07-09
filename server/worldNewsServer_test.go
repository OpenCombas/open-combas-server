package server

import (
	"testing"
	"time"
)

func TestNewsEntryFromEvent(t *testing.T) {
	ts := time.Date(2026, 7, 9, 12, 34, 0, 0, time.UTC).Unix()
	e := newsEntryFromEvent(EventRecord{CreatedAt: ts, TemplateID: 3, EntityID: 75, Text: "hi"})
	if e.TemplateID != 3 || e.EntityID != 75 {
		t.Errorf("template/entity = %d/%d, want 3/75", e.TemplateID, e.EntityID)
	}
	// Code header = [month, day, hour, minute, 0] -> renders as "07/09/75 12:34".
	if e.Code != [5]byte{7, 9, 12, 34, 0} {
		t.Errorf("code = %v, want [7 9 12 34 0]", e.Code)
	}
	if string(e.Text[:2]) != "hi" {
		t.Errorf("text = %q, want hi", string(e.Text[:2]))
	}
}

func TestNewsFromEvents(t *testing.T) {
	// 12 events -> block 0 gets the first 10, block 1 the remaining 2, order preserved (caller passes
	// newest-first).
	evs := make([]EventRecord, 12)
	for i := range evs {
		evs[i] = EventRecord{CreatedAt: int64(1000 - i), TemplateID: int32(i + 1), EntityID: int32(i)}
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

func TestDissolutionTemplate(t *testing.T) {
	for loser, want := range map[byte]int32{'A': 3, 'B': 1, 'C': 2} {
		if got, ok := dissolutionTemplate(loser); !ok || got != want {
			t.Errorf("dissolutionTemplate(%c) = %d,%v want %d,true", loser, got, ok, want)
		}
	}
	if _, ok := dissolutionTemplate('Z'); ok {
		t.Error("dissolutionTemplate('Z') should be false")
	}
}
