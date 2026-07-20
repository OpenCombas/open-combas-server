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
		}, {
			XUID:       "000900006A4B8EEB",
			UserID:     "US0001000000000002",
			Gamertag:   "ibac", // mis-sourced Live gamertag (= the lead's)
			Name:       "notibac",
			UserNumber: 2,
			Rank:       1,
		}},
		Settings: &SquadSettings{
			Stance:    0x04,
			Activity:  0x03,
			Language:  0x43,
			Regions:   0x02,
			RoleFlags: 0x3f,
			Colors:    []byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0xa0, 0xb0, 0xc0},
			Patern:    0x04,
		},
	}

	state := squadLoginStateFromSquad(UserHelloMessage{Xuid: squadXuid}, squad, nil)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)
	body := buf[constants.MinHelloMessageSize:]

	// The five Set-Squad-Profile settings surface at consecutive TeamInfo bytes [77..81]; the 4-colour
	// palette at Color1..4 [64..75] and the palette selector at Patern[76].
	if c := body[64:76]; c[0] != 0x10 || c[3] != 0x40 || c[6] != 0x70 || c[9] != 0xa0 {
		t.Errorf("palette Color1..4 = % x", c)
	}
	if body[76] != 0x04 {
		t.Errorf("Patern = %#x, want 0x04", body[76])
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
	// Offset 18 is the lobby CAPTURE counter, not the roster size -- this call passes nil stats, so 0.
	// The roster size is carried by the member array itself, which the XUID check below covers.
	if body[18] != 0 {
		t.Errorf("capture points = %d, want 0 (nil stats)", body[18])
	}
	if got := int64(binary.LittleEndian.Uint64(body[288:296])); got != 0x000900001AC5EE91 {
		t.Errorf("member XUID = %#x", got)
	}
	if uid := string(body[296:314]); uid != "US0001000000000001" {
		t.Errorf("member UserID = %q", uid)
	}
	// Leader has no in-squad Name -> wire UserName falls back to the gamertag ("ibac").
	if name := string(body[315:319]); name != "ibac" {
		t.Errorf("member[0] UserName = %q, want fallback gamertag 'ibac'", name)
	}
	// Member[1] (at body[288+48=336]) HAS a Name -> wire UserName must be the pilot name
	// ("notibac"), NOT the mis-sourced gamertag ("ibac"). This is what a joining console
	// self-matches on to derive its US/TM for the lobby handshake.
	if uid := string(body[344:362]); uid != "US0001000000000002" {
		t.Errorf("member[1] UserID = %q", uid)
	}
	if name := string(body[363:370]); name != "notibac" {
		t.Errorf("member[1] UserName = %q, want pilot name 'notibac' (not gamertag)", name)
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

// TestAddMemberXUIDAuthoritative verifies the join-commit (code-182) keys idempotency on XUID across the
// WHOLE roster, so a re-commit of an existing member is never rejected by the unreliable name/gamertag
// fields (the host mis-sources them from the squad lead / a prior member). Skipped unless MONGO_TEST_URI
// is set; uses a throwaway database and cleans up after itself.
func TestAddMemberXUIDAuthoritative(t *testing.T) {
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

	leader, _ := repo.EnsureProfile(ctx, "000900000000AAAA", "leader")
	squad, err := repo.CreateSquad(ctx, "Alpha", "B", leader)
	if err != nil {
		t.Fatalf("CreateSquad: %v", err)
	}
	tid := squad.TeamID

	// Add B (name "Bee") then A (name "Ace"), so B precedes A in the roster.
	if st, _, _ := repo.AddMember(ctx, tid, "000900000000BBBB", "gtB", "Bee", 1); st != '1' {
		t.Fatalf("add B: status %q, want '1'", st)
	}
	stA, userA, err := repo.AddMember(ctx, tid, "000900000000ACED", "gtA", "Ace", 1)
	if err != nil || stA != '1' || userA == "" {
		t.Fatalf("add A: status %q userID %q err %v", stA, userA, err)
	}

	// Re-commit A with a MIS-SOURCED name that collides with B (which precedes A in the array). Must
	// still return '1' with A's original User ID -- the pre-fix single loop returned '3' here, wedging
	// the re-commit and blocking the rejoin.
	st, user, err := repo.AddMember(ctx, tid, "000900000000ACED", "wrongGamertag", "Bee", 1)
	if err != nil {
		t.Fatalf("re-commit A: %v", err)
	}
	if st != '1' || user != userA {
		t.Errorf("re-commit A: status %q userID %q, want '1' %q", st, user, userA)
	}

	// The churn re-commit must not create a duplicate roster entry for A.
	sq, _ := repo.SquadByTeamID(ctx, tid)
	countA := 0
	for _, m := range sq.Members {
		if m.XUID == "000900000000ACED" {
			countA++
		}
	}
	if countA != 1 {
		t.Errorf("roster has %d entries for A, want 1", countA)
	}

	// A genuinely-new XUID whose name collides with an existing member is still rejected with '3'.
	if st, _, _ := repo.AddMember(ctx, tid, "000900000000CCCC", "gtC", "Bee", 1); st != '3' {
		t.Errorf("new member with colliding name: status %q, want '3'", st)
	}
}

// TestSquadLoginLobbyCounters pins the two team-header fields the squad lobby renders as Capture and
// Renown. Both were previously misnamed from an empirically-derived field list -- offset 18 was serving the
// roster size and offset 20 the ranking position, so a squad with 444 lifetime renown displayed
// "Renown 1". The offsets are fixed by Release.xex sub_821AF778, which passes hdr+18 to a widget resolved
// as "string_Capture" and hdr+20 to one resolved as "string_Renown".
func TestSquadLoginLobbyCounters(t *testing.T) {
	squad := &Squad{
		TeamID:  "TM0000000001",
		Name:    "TestSquad",
		Faction: "B",
		Rank:    1, // deliberately 1: the old bug served THIS into the Renown field
		Members: []SquadMemberRecord{{XUID: string(squadXuid[:]), Gamertag: "tester", Leader: true}},
	}
	stats := &SquadStats{TeamID: "TM0000000001"}
	stats.Renown.Total = 444
	stats.CapturePoints.Total = 37

	state := squadLoginStateFromSquad(UserHelloMessage{Xuid: squadXuid}, squad, stats)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)
	body := buf[constants.MinHelloMessageSize:]

	if body[18] != 37 {
		t.Errorf("capture points (off 18) = %d, want 37", body[18])
	}
	if got := int32(binary.LittleEndian.Uint32(body[20:24])); got != 444 {
		t.Errorf("renown (off 20) = %d, want 444 (squad.Rank=1 must NOT appear here)", got)
	}
}

// TestSquadLoginCaptureSaturates covers the wire limit: capture points are an int32 in the database but a
// single byte on the wire, so a high total must peg at 255 rather than wrap to a small, plausible-looking
// number.
func TestSquadLoginCaptureSaturates(t *testing.T) {
	squad := &Squad{
		TeamID:  "TM0000000002",
		Name:    "TestSquad",
		Faction: "A",
		Members: []SquadMemberRecord{{XUID: string(squadXuid[:]), Gamertag: "tester", Leader: true}},
	}
	stats := &SquadStats{TeamID: "TM0000000002"}
	stats.CapturePoints.Total = 300 // would wrap to 44 in a plain byte conversion

	state := squadLoginStateFromSquad(UserHelloMessage{Xuid: squadXuid}, squad, stats)
	buf := encodeToBuffer(state, constants.SquadLoginResponseSize, t)
	body := buf[constants.MinHelloMessageSize:]

	if body[18] != 255 {
		t.Errorf("capture points (off 18) = %d, want 255 (saturated, not wrapped)", body[18])
	}
}
