package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

// joinPacket builds a code-182 join request with the comma-joined body the host sends.
func joinPacket(body string) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+len(body)+1)
	if _, err := binary.Encode(pkt, binary.LittleEndian, CreateHeader(squadXuid, [8]byte{})); err != nil {
		panic(err)
	}
	copy(pkt[constants.MinHelloMessageSize:], body)
	return pkt
}

// TestParseSquadJoin decodes the exact body captured from a live host (join_squad_host.pcapng). Crucially
// the %lld joiner XUID (decimal 2533275239575185) must convert to the 16-char upper-hex 000900001AC5EE91,
// the same key the joiner's own message headers use.
func TestParseSquadJoin(t *testing.T) {
	pkt := joinPacket("notibac,TM0001000000000001,2533275239575185,ibac,34,0")
	gt, team, xuid, name, rank, ok := parseSquadJoin(pkt)
	if !ok {
		t.Fatal("parse failed")
	}
	if gt != "notibac" || team != "TM0001000000000001" || xuid != "000900001AC5EE91" || name != "ibac" || rank != 34 {
		t.Errorf("parse = (%q,%q,%q,%q,%d)", gt, team, xuid, name, rank)
	}
}

func TestParseSquadJoinShortBody(t *testing.T) {
	if _, _, _, _, _, ok := parseSquadJoin(joinPacket("notibac,TM0001")); ok {
		t.Error("expected short body to fail parse")
	}
}

// TestSquadJoinRoundTrip verifies the 40-byte record lands where sub_823BDAA0 reads it: status at body[0],
// User ID at body[2..20), total wire size 72.
func TestSquadJoinRoundTrip(t *testing.T) {
	state := squadJoinState(squadXuid, [8]byte{}, '1', "US0001000000000042")
	buf := make([]byte, constants.SquadJoinResponseSize)
	if _, err := binary.Encode(buf, binary.LittleEndian, state); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf) != 72 {
		t.Fatalf("size = %d, want 72", len(buf))
	}
	body := buf[constants.MinHelloMessageSize:]
	if body[0] != '1' {
		t.Errorf("status = %q, want '1'", body[0])
	}
	if got := string(body[2:20]); got != "US0001000000000042" {
		t.Errorf("userID = %q", got)
	}
}

// TestAddMemberLive exercises the join-commit outcomes against a real MongoDB. Skipped without one.
func TestAddMemberLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live join test")
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

	leader, _ := repo.EnsureProfile(ctx, "000900001C1F0771", "hostguy")
	squad, _ := repo.CreateSquad(ctx, "OpenCombas", "B", leader)

	// Unknown squad -> '0' (Unknown Error), no id.
	if st, id, _ := repo.AddMember(ctx, "TM9999999999999999", "000900001AC5EE91", "notibac", "ibac", 34); st != '0' || id != "" {
		t.Errorf("unknown squad = (%q,%q), want ('0',\"\")", st, id)
	}

	// Join a new member -> '1' + minted User ID, roster grows to 2. Note the joiner shares the leader's
	// gamertag "notibac" (two consoles, distinct XUIDs) but a distinct in-squad name "ibac" -> must succeed.
	st, id, err := repo.AddMember(ctx, squad.TeamID, "000900001AC5EE91", "notibac", "ibac", 34)
	if err != nil || st != '1' || id == "" {
		t.Fatalf("join = (%q,%q,%v), want ('1',<id>,nil)", st, id, err)
	}
	got, _ := repo.SquadByTeamID(ctx, squad.TeamID)
	if got == nil || len(got.Members) != 2 {
		t.Fatalf("roster = %+v, want 2 members", got)
	}
	if m := got.Members[1]; m.XUID != "000900001AC5EE91" || m.UserID != id || m.Gamertag != "notibac" || m.Name != "ibac" || m.Leader {
		t.Errorf("member = %+v", m)
	}
	// Joiner's profile is now linked to the team.
	if p, _ := repo.EnsureProfile(ctx, "000900001AC5EE91", "notibac"); p.TeamID != squad.TeamID {
		t.Errorf("profile team = %q, want %q", p.TeamID, squad.TeamID)
	}

	// Idempotent: re-joining returns the same id, no duplicate.
	if st2, id2, _ := repo.AddMember(ctx, squad.TeamID, "000900001AC5EE91", "notibac", "ibac", 34); st2 != '1' || id2 != id {
		t.Errorf("re-join = (%q,%q), want ('1',%q)", st2, id2, id)
	}
	if got, _ := repo.SquadByTeamID(ctx, squad.TeamID); len(got.Members) != 2 {
		t.Errorf("re-join grew roster to %d", len(got.Members))
	}

	// Name collision: a different XUID taking the same in-squad name "ibac" -> '3'.
	if st, _, _ := repo.AddMember(ctx, squad.TeamID, "000900000000BEEF", "someguy", "ibac", 1); st != '3' {
		t.Errorf("name collision = %q, want '3'", st)
	}

	// Fill to the 20-member cap (distinct names), then the next join is rejected '2' (Member Number Over).
	for i := len(got.Members); i < squadMemberCap; i++ {
		suffix := string(rune('A' + i))
		xuid := "0009000000000" + suffix + "00"
		if _, _, err := repo.AddMember(ctx, squad.TeamID, xuid, "pilot"+suffix, "nm"+suffix, 1); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if st, _, _ := repo.AddMember(ctx, squad.TeamID, "000900000000FF00", "overflow", "overflowname", 1); st != '2' {
		t.Errorf("full squad = %q, want '2'", st)
	}
}
