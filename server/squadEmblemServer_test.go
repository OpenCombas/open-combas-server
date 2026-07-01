package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

func emblemPacket(teamID string, emblems []byte) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+squadEmblemBodySize)
	if _, err := binary.Encode(pkt, binary.LittleEndian, CreateHeader(squadXuid, [8]byte{})); err != nil {
		panic(err)
	}
	body := pkt[constants.MinHelloMessageSize:]
	copy(body[0:], "ibac")
	copy(body[16:], teamID)
	copy(body[squadEmblemOffset:], emblems)
	return pkt
}

func TestParseSquadEmblem(t *testing.T) {
	// emblem[0]: PaternID=12, Color=2 (captured layer values)
	emblems := make([]byte, squadEmblemBlobSize)
	emblems[0] = 0x0c
	emblems[8] = 0x02
	team, got, ok := parseSquadEmblem(emblemPacket("TM0001000000000001", emblems))
	if !ok {
		t.Fatal("parse failed")
	}
	if team != "TM0001000000000001" {
		t.Errorf("team = %q", team)
	}
	if len(got) != squadEmblemBlobSize || got[0] != 0x0c || got[8] != 0x02 {
		t.Errorf("emblem blob wrong: len=%d [0]=%#x [8]=%#x", len(got), got[0], got[8])
	}
}

// TestSquadEmblemRoundTrip proves a stored emblem blob surfaces at the emblem array of the squad-login
// response (SquadData.Emblems begins at body offset 92, right after the 92-byte TeamInfo).
func TestSquadEmblemRoundTrip(t *testing.T) {
	emblems := make([]byte, squadEmblemBlobSize)
	emblems[0] = 0x0c // emblem[0] PaternID low byte
	emblems[8] = 0x02 // emblem[0] Color

	squad := &Squad{TeamID: "TM0001000000000001", Name: "OpenCombas", Faction: "B", Emblems: emblems}
	state := squadLoginStateFromSquad(UserHelloMessage{Xuid: squadXuid}, squad)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)
	body := buf[constants.MinHelloMessageSize:]

	const emblemBase = 92 // after TeamInfo(92)
	if body[emblemBase] != 0x0c {
		t.Errorf("emblem[0] PaternID = %#x, want 0x0c", body[emblemBase])
	}
	if body[emblemBase+8] != 0x02 {
		t.Errorf("emblem[0] Color = %#x, want 0x02", body[emblemBase+8])
	}
}

func TestSetEmblemsLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live emblem test")
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

	emblems := make([]byte, squadEmblemBlobSize)
	emblems[0] = 0x0c
	if found, err := repo.SetEmblems(ctx, squad.TeamID, emblems); err != nil || !found {
		t.Fatalf("SetEmblems: found=%v err=%v", found, err)
	}
	if got, _ := repo.SquadByTeamID(ctx, squad.TeamID); got == nil || len(got.Emblems) != squadEmblemBlobSize || got.Emblems[0] != 0x0c {
		t.Errorf("emblem not persisted: %+v", got)
	}
	// Unknown team -> not found.
	if found, _ := repo.SetEmblems(ctx, "TM9999999999999999", emblems); found {
		t.Error("emblem set on unknown team reported found")
	}
}
