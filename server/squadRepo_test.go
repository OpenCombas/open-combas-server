package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

// TestFormatIDs locks the id format and the continuity property: the first squad/user (seq 1) get the
// same ids the server used to hard-code, so an existing OnlineProfile.bin still lines up.
func TestFormatIDs(t *testing.T) {
	if got := formatTeamID(1); got != "TM0001000000000001" {
		t.Errorf("formatTeamID(1) = %q", got)
	}
	if got := formatUserID(1); got != "US0001000000000001" {
		t.Errorf("formatUserID(1) = %q", got)
	}
	if got := formatTeamID(42); got != "TM0001000000000042" {
		t.Errorf("formatTeamID(42) = %q", got)
	}
	if len(formatTeamID(1)) != 18 || len(formatUserID(1)) != 18 {
		t.Error("ids must be 18 chars to fit the 18-byte wire fields")
	}
}

// TestSquadLoginStateFromSquad proves a persisted squad serialises to the exact offsets the client
// parser (sub_823BCD88) reads, with the roster coming from the stored members rather than placeholders.
func TestSquadLoginStateFromSquad(t *testing.T) {
	squad := &Squad{
		TeamID:  "TM0001000000000001",
		Name:    "OpenCombas",
		Faction: "B",
		Rank:    1,
		Members: []SquadMemberRecord{{
			XUID:       "000900001AC5EE91",
			UserID:     "US0001000000000001",
			Gamertag:   "ibac",
			Leader:     true,
			UserNumber: 1,
			Rank:       1,
		}},
		Settings: &SquadSettings{
			Stance:    0x04,
			Activity:  0x03,
			Language:  0x43,
			Regions:   0x02,
			RoleFlags: 0x3f,
			Colors:    []byte{0x10, 0x20, 0x30},
		},
	}

	state := squadLoginStateFromSquad(UserHelloMessage{Xuid: squadXuid}, squad)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)
	body := buf[constants.MinHelloMessageSize:]

	// The five Set-Squad-Profile settings surface at consecutive TeamInfo bytes [77..81]; colours at Color1.
	if c := body[64:67]; c[0] != 0x10 || c[1] != 0x20 || c[2] != 0x30 {
		t.Errorf("Color1 = % x, want 10 20 30", c)
	}
	if body[77] != 0x04 {
		t.Errorf("stance[77] = %#x, want 0x04", body[77])
	}
	if body[78] != 0x03 {
		t.Errorf("activity[78] = %#x, want 0x03", body[78])
	}
	if body[79] != 0x43 {
		t.Errorf("language[79] = %#x, want 0x43", body[79])
	}
	if body[80] != 0x02 {
		t.Errorf("regions[80] = %#x, want 0x02", body[80])
	}
	if body[81] != 0x3f {
		t.Errorf("roleFlags[81] = %#x, want 0x3f", body[81])
	}

	if body[0] != 0 {
		t.Errorf("status = %d, want 0 (valid team)", body[0])
	}
	if name := string(body[1:11]); name != "OpenCombas" {
		t.Errorf("team name = %q", name)
	}
	if body[17] != 'B' {
		t.Errorf("country code = %q, want 'B'", body[17])
	}
	if body[18] != 1 {
		t.Errorf("member count = %d, want 1", body[18])
	}
	if got := int64(binary.LittleEndian.Uint64(body[288:296])); got != 0x000900001AC5EE91 {
		t.Errorf("member XUID = %#x", got)
	}
	if uid := string(body[296:314]); uid != "US0001000000000001" {
		t.Errorf("member UserID = %q", uid)
	}
	if name := string(body[315:319]); name != "ibac" {
		t.Errorf("member UserName = %q", name)
	}
	if body[332] != 1 {
		t.Errorf("leader flag = %d, want 1", body[332])
	}
	if r := body[333:336]; r[0] != 0 || r[1] != 0 || r[2] != 1 {
		t.Errorf("rank bytes = % x, want 00 00 01", r)
	}
}

func TestNoTeamLoginState(t *testing.T) {
	state := noTeamLoginState(UserHelloMessage{Xuid: squadXuid})
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)
	if buf[constants.MinHelloMessageSize] == 0 {
		t.Error("no-team status byte must be non-zero")
	}
}

// TestSquadRepositoryLive exercises the full reg/login persistence path against a real MongoDB. Skipped
// unless MONGO_TEST_URI is set; uses a throwaway database and cleans up after itself.
func TestSquadRepositoryLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live squad-repository test")
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

	// First profile gets US...0001 and is stable across calls.
	p1, err := repo.EnsureProfile(ctx, "000900001AC5EE91", "ibac")
	if err != nil {
		t.Fatalf("EnsureProfile: %v", err)
	}
	if p1.UserID != "US0001000000000001" {
		t.Errorf("first UserID = %q, want US0001000000000001", p1.UserID)
	}
	if p2, _ := repo.EnsureProfile(ctx, "000900001AC5EE91", "ibac"); p2.UserID != p1.UserID {
		t.Errorf("EnsureProfile not idempotent: %q vs %q", p2.UserID, p1.UserID)
	}

	// First squad gets TM...0001 with the creator as leader.
	squad, err := repo.CreateSquad(ctx, "OpenCombas", "B", p1)
	if err != nil {
		t.Fatalf("CreateSquad: %v", err)
	}
	if squad.TeamID != "TM0001000000000001" {
		t.Errorf("first TeamID = %q, want TM0001000000000001", squad.TeamID)
	}
	if len(squad.Members) != 1 || !squad.Members[0].Leader || squad.Members[0].UserID != p1.UserID {
		t.Errorf("unexpected leader member: %+v", squad.Members)
	}

	// Round-trips by id and name; unknown lookups are (nil, nil).
	if got, _ := repo.SquadByTeamID(ctx, squad.TeamID); got == nil || got.Name != "OpenCombas" || got.Faction != "B" {
		t.Errorf("SquadByTeamID round-trip failed: %+v", got)
	}
	if got, _ := repo.SquadByName(ctx, "OpenCombas"); got == nil {
		t.Error("SquadByName should find the squad")
	}
	if got, _ := repo.SquadByName(ctx, "Nonexistent"); got != nil {
		t.Error("SquadByName should return nil for unknown name")
	}
}
