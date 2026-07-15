package server

import "testing"

// synthetic ids (no real gamertags/log data — open-source project).
const (
	tmA = "TM0001000000000010"
	tmB = "TM0001000000000020"
	tmC = "TM0001000000000030"
	xP  = "000900000000AA01"
)

func dispKind(p playerPlan, teamID string) string {
	for _, d := range p.Dispositions {
		if d.TeamID == teamID {
			return d.Kind
		}
	}
	return ""
}

func TestChooseKeepSquad(t *testing.T) {
	ms := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 1},
		{TeamID: tmB, Leader: false, Size: 3},
	}
	// 1. profile pointer wins when the player is actually a member of it.
	if got := chooseKeepSquad(ms, tmB); got != tmB {
		t.Errorf("profile-team keep = %q, want %q", got, tmB)
	}
	// 2. profile pointer at a squad they're NOT in -> fall to the single led squad.
	if got := chooseKeepSquad(ms, "TM0001000000000099"); got != tmA {
		t.Errorf("sole-leader keep = %q, want %q", got, tmA)
	}
	// 3. no profile match, leads none -> most-recent (highest teamId).
	ms2 := []squadMembership{{TeamID: tmA, Size: 3}, {TeamID: tmC, Size: 2}}
	if got := chooseKeepSquad(ms2, ""); got != tmC {
		t.Errorf("most-recent keep = %q, want %q", got, tmC)
	}
}

func TestPlanPlayerDispositions(t *testing.T) {
	// Player is: leader of a SOLO squad A, a non-leader member of B, and leader of C which has followers.
	// Home (profile) points at B, so B is kept.
	ms := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 1},  // orphaned solo squad -> disband
		{TeamID: tmB, Leader: false, Size: 4}, // home -> keep
		{TeamID: tmC, Leader: true, Size: 3},  // leads with followers -> flag (manual)
	}
	p := planPlayer(xP, ms, tmB)
	if p.Keep != tmB {
		t.Fatalf("keep = %q, want %q", p.Keep, tmB)
	}
	if k := dispKind(p, tmB); k != "keep" {
		t.Errorf("%s disposition = %q, want keep", tmB, k)
	}
	if k := dispKind(p, tmA); k != "disband" {
		t.Errorf("%s disposition = %q, want disband", tmA, k)
	}
	if k := dispKind(p, tmC); k != "flag" {
		t.Errorf("%s disposition = %q, want flag", tmC, k)
	}
}

func TestPlanPlayerPullsNonLeaderFromExtra(t *testing.T) {
	// Leader of A (kept), plain member of B -> B is pulled.
	ms := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 1},
		{TeamID: tmB, Leader: false, Size: 5},
	}
	p := planPlayer(xP, ms, tmA)
	if p.Keep != tmA {
		t.Fatalf("keep = %q, want %q", p.Keep, tmA)
	}
	if k := dispKind(p, tmB); k != "pull" {
		t.Errorf("%s disposition = %q, want pull", tmB, k)
	}
}
