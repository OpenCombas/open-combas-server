package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"os"
	"testing"
)

func TestParseSquadWithdraw(t *testing.T) {
	pkt := makeRequestPacket(squadXuid, [8]byte{}, "ibacsqtest,TM0001000000000008,US0001000000000008")
	gt, team, user := parseSquadWithdraw(pkt)
	if gt != "ibacsqtest" || team != "TM0001000000000008" || user != "US0001000000000008" {
		t.Errorf("parse = (%q,%q,%q)", gt, team, user)
	}
}

// TestRemoveMemberLive covers the withdraw status outcomes against a real MongoDB. Skipped without it.
func TestRemoveMemberLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live withdraw test")
	}
	ctx := context.Background()

	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewSquadRepository(store)
	_ = repo.squads.Drop(ctx)
	_ = repo.profiles.Drop(ctx)
	_ = repo.counters.Drop(ctx)
	t.Cleanup(func() {
		_ = repo.squads.Drop(ctx)
		_ = repo.profiles.Drop(ctx)
		_ = repo.counters.Drop(ctx)
	})
	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	// Unknown team -> '3'.
	if st, _ := repo.RemoveMember(ctx, "TM9999999999999999", "US0001000000000001"); st != '3' {
		t.Errorf("unknown team status = %q, want '3'", st)
	}

	leader, _ := repo.EnsureProfile(ctx, "000900001AC5EE91", "ibac")
	squad, _ := repo.CreateSquad(ctx, "OpenCombas", "B", leader)

	// Leader with another member present -> '2'.
	if _, err := repo.squads.UpdateOne(ctx,
		map[string]any{"teamId": squad.TeamID},
		map[string]any{"$push": map[string]any{"members": SquadMemberRecord{XUID: "AAAA", UserID: "US0001000000000002", Gamertag: "buddy"}}},
	); err != nil {
		t.Fatalf("seed second member: %v", err)
	}
	if st, _ := repo.RemoveMember(ctx, squad.TeamID, leader.UserID); st != '2' {
		t.Errorf("leader-with-members status = %q, want '2'", st)
	}

	// Non-leader member leaves -> '1', squad survives.
	if st, _ := repo.RemoveMember(ctx, squad.TeamID, "US0001000000000002"); st != '1' {
		t.Errorf("member leave status = %q, want '1'", st)
	}
	if got, _ := repo.SquadByTeamID(ctx, squad.TeamID); got == nil || len(got.Members) != 1 {
		t.Errorf("squad should survive with 1 member, got %+v", got)
	}

	// Solo leader leaves -> '1', squad disbands and profile unlinked.
	if st, _ := repo.RemoveMember(ctx, squad.TeamID, leader.UserID); st != '1' {
		t.Errorf("solo leader leave status = %q, want '1'", st)
	}
	if got, _ := repo.SquadByTeamID(ctx, squad.TeamID); got != nil {
		t.Error("squad should be disbanded after last member leaves")
	}
}

func TestSquadWithdrawAckSize(t *testing.T) {
	state := squadAckState(squadXuid, [8]byte{}, '1')
	buf := encodeToBuffer(state, constants.SquadAckResponseSize, t)
	if len(buf) != 34 || buf[constants.MinHelloMessageSize] != '1' {
		t.Errorf("withdraw ack size/status wrong: len=%d status=%q", len(buf), buf[constants.MinHelloMessageSize])
	}
}
