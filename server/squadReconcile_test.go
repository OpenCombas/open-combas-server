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
	// 1. profile pointer wins when the player is a member of it (their led squad is solo, so no override).
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
	// 4. LEADER WITH FOLLOWERS WINS: leading a squad with followers outranks the profile pointer.
	ms3 := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 3},  // leads, has followers
		{TeamID: tmB, Leader: false, Size: 4}, // profile points here
	}
	if got := chooseKeepSquad(ms3, tmB); got != tmA {
		t.Errorf("led-with-followers keep = %q, want %q", got, tmA)
	}
	// 5. several led-with-followers squads: the profile pointer breaks the tie when it's one of them...
	ms4 := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 3},
		{TeamID: tmC, Leader: true, Size: 2},
	}
	if got := chooseKeepSquad(ms4, tmA); got != tmA {
		t.Errorf("led-with-followers profile tiebreak = %q, want %q", got, tmA)
	}
	// ...else the most recent led-with-followers wins.
	if got := chooseKeepSquad(ms4, ""); got != tmC {
		t.Errorf("led-with-followers recency tiebreak = %q, want %q", got, tmC)
	}
}

func TestPlanPlayerDispositions(t *testing.T) {
	// Player is: leader of a SOLO squad A, a non-leader member of B (their profile home), and leader of C
	// which has followers. Leader-with-followers wins: C is kept, B is pulled, solo A is disbanded.
	ms := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 1},  // orphaned solo squad -> disband
		{TeamID: tmB, Leader: false, Size: 4}, // stale home -> pull
		{TeamID: tmC, Leader: true, Size: 3},  // leads with followers -> keep
	}
	p := planPlayer(xP, ms, tmB)
	if p.Keep != tmC {
		t.Fatalf("keep = %q, want %q", p.Keep, tmC)
	}
	if k := dispKind(p, tmC); k != "keep" {
		t.Errorf("%s disposition = %q, want keep", tmC, k)
	}
	if k := dispKind(p, tmA); k != "disband" {
		t.Errorf("%s disposition = %q, want disband", tmA, k)
	}
	if k := dispKind(p, tmB); k != "pull" {
		t.Errorf("%s disposition = %q, want pull", tmB, k)
	}
}

func TestPlanPlayerFlagsSecondLedSquad(t *testing.T) {
	// A player leading TWO squads that both have followers is genuinely ambiguous: one is kept (most
	// recent, no profile match), the other is flagged — a followed leader is never auto-pulled.
	ms := []squadMembership{
		{TeamID: tmA, Leader: true, Size: 3},
		{TeamID: tmC, Leader: true, Size: 2},
	}
	p := planPlayer(xP, ms, "")
	if p.Keep != tmC {
		t.Fatalf("keep = %q, want %q", p.Keep, tmC)
	}
	if k := dispKind(p, tmA); k != "flag" {
		t.Errorf("%s disposition = %q, want flag", tmA, k)
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
