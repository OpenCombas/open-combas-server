package server

import "testing"

// Synthetic ids/tags only (no real gamertags/log data — open-source project).
func TestClobberedTagFixes(t *testing.T) {
	sq := Squad{
		TeamID: "TM0001000000000010",
		Members: []SquadMemberRecord{
			// The host/leader: owns the duplicated tag (empty join name, reg path) — never touched.
			{XUID: "000900000000AA01", Gamertag: "Pilot-Host", Name: "", Leader: true},
			// Clobbered joiners: host's tag stamped on, own join name differs -> fixed from the name.
			{XUID: "000900000000AA02", Gamertag: "Pilot-Host", Name: "Pilot-Two"},
			{XUID: "000900000000AA03", Gamertag: "Pilot-Host", Name: "Pilot-Three"},
			// Owner-looking row inside a dup group (name == tag) — never touched.
			{XUID: "000900000000AA04", Gamertag: "Pilot-Four", Name: "Pilot-Four"},
			{XUID: "000900000000AA05", Gamertag: "Pilot-Four", Name: "Pilot-Five"},
			// Mismatch WITHOUT an in-roster duplicate — unprovable, left for login self-heal.
			{XUID: "000900000000AA06", Gamertag: "Pilot-Solo", Name: "Pilot-Other"},
			// In a dup group but covered by the authoritative players pass -> skipped here.
			{XUID: "000900000000AA07", Gamertag: "Pilot-Host", Name: "Pilot-Seven"},
		},
	}
	truth := map[string]string{"000900000000AA07": "Pilot-Seven"}

	fixes := clobberedTagFixes(sq, truth)
	got := map[string]clobberFix{}
	for _, f := range fixes {
		got[f.XUID] = f
	}
	if len(fixes) != 3 {
		t.Fatalf("fixes = %d (%v), want 3", len(fixes), got)
	}
	for xuid, want := range map[string]string{
		"000900000000AA02": "Pilot-Two",
		"000900000000AA03": "Pilot-Three",
		"000900000000AA05": "Pilot-Five",
	} {
		f, ok := got[xuid]
		if !ok || f.New != want || f.Old == "" || f.TeamID != sq.TeamID {
			t.Errorf("%s fix = %+v, want New=%q", xuid, f, want)
		}
	}
	for _, skipped := range []string{"000900000000AA01", "000900000000AA04", "000900000000AA06", "000900000000AA07"} {
		if _, ok := got[skipped]; ok {
			t.Errorf("%s must not be fixed (owner/unprovable/authoritative)", skipped)
		}
	}
}
