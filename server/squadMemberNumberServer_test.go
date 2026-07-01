package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

// memberNumberPacket builds a 57-byte hound-number request matching the capture layout.
func memberNumberPacket(gamertag, teamID, userID string, number byte) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+squadMemberNumberBodySize)
	if _, err := binary.Encode(pkt, binary.LittleEndian, CreateHeader(squadXuid, [8]byte{})); err != nil {
		panic(err)
	}
	body := pkt[constants.MinHelloMessageSize:]
	copy(body[0:], gamertag)
	copy(body[16:], teamID)
	copy(body[35:], userID)
	body[55] = number
	return pkt
}

func TestParseSquadMemberNumber(t *testing.T) {
	pkt := memberNumberPacket("ibacsqtest", "TM0001000000000009", "US0001000000000008", 0x08)
	gt, team, user, num, ok := parseSquadMemberNumber(pkt)
	if !ok {
		t.Fatal("parse failed")
	}
	if gt != "ibacsqtest" || team != "TM0001000000000009" || user != "US0001000000000008" || num != 8 {
		t.Errorf("parse = (%q,%q,%q,%d)", gt, team, user, num)
	}
}

// TestSetMemberNumberLive covers the status outcomes against a real MongoDB. Skipped without it.
func TestSetMemberNumberLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live member-number test")
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

	leader, _ := repo.EnsureProfile(ctx, "000900001AC5EE91", "ibac")
	squad, _ := repo.CreateSquad(ctx, "OpenCombas", "B", leader)

	// Unknown team -> '3'.
	if st, _ := repo.SetMemberNumber(ctx, "TM9999999999999999", leader.UserID, 8); st != '3' {
		t.Errorf("unknown team = %q, want '3'", st)
	}
	// Assign 8 to the leader -> '1', persisted.
	if st, _ := repo.SetMemberNumber(ctx, squad.TeamID, leader.UserID, 8); st != '1' {
		t.Errorf("assign = %q, want '1'", st)
	}
	if got, _ := repo.SquadByTeamID(ctx, squad.TeamID); got == nil || got.Members[0].UserNumber != 8 {
		t.Errorf("number not persisted: %+v", got)
	}

	// Add a second member holding number 8; the leader re-claiming 8 is fine (same member), but a third
	// member taking 8 collides.
	if _, err := repo.squads.UpdateOne(ctx,
		map[string]any{"teamId": squad.TeamID},
		map[string]any{"$push": map[string]any{"members": SquadMemberRecord{XUID: "BBBB", UserID: "US0001000000000099", UserNumber: 8}}},
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if st, _ := repo.SetMemberNumber(ctx, squad.TeamID, leader.UserID, 8); st != '2' {
		t.Errorf("collision = %q, want '2'", st)
	}
	// Unknown member -> '3'.
	if st, _ := repo.SetMemberNumber(ctx, squad.TeamID, "US0000000000000000", 5); st != '3' {
		t.Errorf("unknown member = %q, want '3'", st)
	}
}
