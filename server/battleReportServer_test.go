package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

// TestBattleReportAck verifies the mission-result ack: the parser sub_823BD940 reads the status at
// body[0] and conquest-event flags at body[2]/[4]/[6]. The server signals "Normal End" with no special
// events ("1,0,0,0").
func TestBattleReportAck(t *testing.T) {
	state := CreateBattleReportAck(squadXuid, [8]byte{})
	buf := encodeToBuffer(state, constants.BattleReportResponseSize, t)

	if len(buf) != 39 {
		t.Fatalf("response size = %d, want 39", len(buf))
	}

	body := buf[constants.MinHelloMessageSize:]
	if body[0] != '1' {
		t.Errorf("status (body[0]) = %q, want '1' (Normal End)", body[0])
	}
	for _, off := range []int{2, 4, 6} {
		if body[off] != '0' {
			t.Errorf("event flag body[%d] = %q, want '0' (no event)", off, body[off])
		}
	}
	for _, off := range []int{1, 3, 5} {
		if body[off] != ',' {
			t.Errorf("separator body[%d] = %q, want ','", off, body[off])
		}
	}
}

// buildReport constructs a synthetic 1254 packet (header + 565-byte body) with the ingestable fields set
// at their real offsets (see SQUAD_STATS_DESIGN.md).
func buildReport(area, mapID, natA byte, teamA string, natB byte, teamB string, merit0, merit1, winner, occ byte) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+battleReportBodySize)
	if _, err := binary.Encode(pkt, binary.LittleEndian, CreateHeader(squadXuid, [8]byte{})); err != nil {
		panic(err)
	}
	b := pkt[constants.MinHelloMessageSize:]
	b[0x23], b[0x24] = area, mapID
	b[0x25] = natA
	copy(b[0x26:], teamA)
	b[0x11C] = merit0
	b[0x12C] = natB
	copy(b[0x12D:], teamB)
	b[0x223] = merit1
	b[0x233], b[0x234] = winner, occ
	return pkt
}

func TestParseBattleReport(t *testing.T) {
	// game-3 shape: nation A (CombasTest/TM…6) beats nation C (CombasKillaz/TM…5); merit 148 on team-1 slot.
	r, ok := parseBattleReport(buildReport(20, 2, 'C', "TM0001000000000005", 'A', "TM0001000000000006", 0, 148, 'A', 99))
	if !ok {
		t.Fatal("parse failed")
	}
	if r.AreaID != 20 || r.MapID != 2 || r.WinnerNation != 'A' || r.LoserNation != 'C' ||
		r.WinnerTeam != "TM0001000000000006" || r.LoserTeam != "TM0001000000000005" ||
		r.OccDelta != 99 || r.WinnerMerit != 148 {
		t.Errorf("game3 parse = %+v", r)
	}

	// game-2 shape: nation C wins; merit 150 on the team-0 slot.
	r2, _ := parseBattleReport(buildReport(20, 2, 'C', "TM0001000000000005", 'A', "TM0001000000000006", 150, 0, 'C', 100))
	if r2.WinnerTeam != "TM0001000000000005" || r2.WinnerMerit != 150 || r2.OccDelta != 100 {
		t.Errorf("game2 parse = %+v", r2)
	}

	// winner nation matching neither block -> not ok.
	if _, ok := parseBattleReport(buildReport(20, 2, 'C', "TM5", 'A', "TM6", 0, 0, 'B', 50)); ok {
		t.Error("expected parse failure when winner matches no block")
	}
	// too-short packet -> not ok.
	if _, ok := parseBattleReport(make([]byte, 100)); ok {
		t.Error("expected parse failure on short packet")
	}
}

// TestPvPAndFierce covers PvP detection and the fierce-battle threshold.
func TestPvPAndFierce(t *testing.T) {
	if !isPvP(BattleResult{WinnerTeam: "TM0001000000000001", LoserTeam: "TM0001000000000002"}) {
		t.Error("two distinct TM squads should be PvP")
	}
	if isPvP(BattleResult{WinnerTeam: "TM0001000000000001", LoserTeam: "AAA9999999999999999"}) {
		t.Error("a battle vs a non-TM (AI/CPU) side is not PvP")
	}
	if isPvP(BattleResult{WinnerTeam: "TM0001000000000001", LoserTeam: "TM0001000000000001"}) {
		t.Error("identical team ids are not two distinct squads")
	}
	// Fierce = strictly OVER 30% PvP.
	if fierceFromCounts(0, 0) {
		t.Error("no battles -> not fierce")
	}
	if fierceFromCounts(10, 3) {
		t.Error("exactly 30% is not over 30%")
	}
	if !fierceFromCounts(10, 4) {
		t.Error("40% PvP should be fierce")
	}
	if !fierceFromCounts(3, 1) {
		t.Error("33% PvP should be fierce")
	}
}

// TestLeadAfterDelta covers the capture trigger: a battlefield changes hands only when a mission's
// occupation shift makes the attacker overtake the holder -- a single mission never flips a full one.
func TestLeadAfterDelta(t *testing.T) {
	const cap = 1250
	// Fresh battlefield fully held by A: one B win (+16) only chips occupation; lead stays A.
	if got := leadAfterDelta(Battlefield{Capacity: cap, OccA: cap}, 'B', 'A', 16); got != 'A' {
		t.Errorf("fresh bf, one B win -> lead %q, want 'A' (no single-mission flip)", got)
	}
	// Contested at the boundary: a B win that pulls it ahead flips the lead (crossover -> capture).
	if got := leadAfterDelta(Battlefield{Capacity: cap, OccA: 630, OccB: 620}, 'B', 'A', 16); got != 'B' {
		t.Errorf("B overtakes -> lead %q, want 'B' (crossover)", got)
	}
	// A B win that's still short of overtaking leaves the lead with A.
	if got := leadAfterDelta(Battlefield{Capacity: cap, OccA: 660, OccB: 620}, 'B', 'A', 16); got != 'A' {
		t.Errorf("B short of overtaking -> lead %q, want 'A'", got)
	}
	// Successful defence (holder A wins) keeps A and never flips.
	if got := leadAfterDelta(Battlefield{Capacity: cap, OccA: 700, OccB: 550}, 'A', 'B', 16); got != 'A' {
		t.Errorf("A defends -> lead %q, want 'A'", got)
	}
}

// TestBattleReportIngestLive exercises the winner-takes-all capture + lock primitives and squad-stat
// accumulation against a real Mongo.
func TestBattleReportIngestLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live battle-report ingest test")
	}
	ctx := context.Background()
	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	world := NewWorldRepository(store)
	squad := NewSquadRepository(store)
	_ = world.battlefields.Drop(ctx)
	_ = world.nations.Drop(ctx)
	_ = squad.stats.Drop(ctx)
	t.Cleanup(func() {
		_ = world.battlefields.Drop(ctx)
		_ = world.nations.Drop(ctx)
		_ = squad.stats.Drop(ctx)
	})

	// --- capture (winner-takes-all + lock) ---
	// Seed a battlefield fully held by nation C. A captures it -> 100% A, and C is locked out until it
	// has fought UnlockBattleThreshold more battles.
	if _, err := world.battlefields.InsertOne(ctx, Battlefield{AreaID: 20, MapID: 2, Capacity: 20000, OccC: 20000}); err != nil {
		t.Fatalf("seed bf: %v", err)
	}
	if err := world.CaptureBattlefield(ctx, 20, 2, 'A', 'C', 10); err != nil {
		t.Fatalf("CaptureBattlefield: %v", err)
	}
	if bf, _ := world.BattlefieldByAreaMap(ctx, 20, 2); bf == nil || bf.OccA != 20000 || bf.OccC != 0 || !bf.Locked || bf.DefeatedNation != "C" || bf.UnlockAtBattle != 10 {
		t.Errorf("capture = %+v, want occA=20000 occC=0 locked defeated=C unlock@10", bf)
	}

	// The lock holds while C is short of the threshold, then a sweep at/after it reopens the battlefield.
	if err := world.UnlockExpiredBattlefields(ctx, 'C', 9); err != nil {
		t.Fatal(err)
	}
	if bf, _ := world.BattlefieldByAreaMap(ctx, 20, 2); bf == nil || !bf.Locked {
		t.Errorf("battlefield should stay locked at C count 9: %+v", bf)
	}
	if err := world.UnlockExpiredBattlefields(ctx, 'C', 10); err != nil {
		t.Fatal(err)
	}
	if bf, _ := world.BattlefieldByAreaMap(ctx, 20, 2); bf == nil || bf.Locked || bf.DefeatedNation != "" || bf.UnlockAtBattle != 0 {
		t.Errorf("battlefield should reopen (lock cleared) at C count 10: %+v", bf)
	}

	// --- battle counter (drives the unlock clock) ---
	if _, err := world.nations.InsertOne(ctx, NationRecord{CountryCode: "A"}); err != nil {
		t.Fatalf("seed nation: %v", err)
	}
	if n, err := world.IncrementBattleCount(ctx, 'A'); err != nil || n != 1 {
		t.Errorf("IncrementBattleCount #1 = %d, %v; want 1", n, err)
	}
	if n, _ := world.IncrementBattleCount(ctx, 'A'); n != 2 {
		t.Errorf("IncrementBattleCount #2 = %d; want 2", n)
	}
	if n, _ := world.NationBattleCount(ctx, 'A'); n != 2 {
		t.Errorf("NationBattleCount = %d; want 2", n)
	}

	// --- squad-stat accumulation ---
	win, lose := "TM0001000000000006", "TM0001000000000005"
	if err := squad.CreditBattle(ctx, win, lose, 99, 148, "0001"); err != nil {
		t.Fatalf("CreditBattle: %v", err)
	}
	st, _ := squad.SquadStatsByTeamID(ctx, win)
	if st == nil || st.CapturePoints.Total != 99 || st.Renown.Total != 148 ||
		st.CapturePoints.BySeason["0001"] != 99 || st.Renown.BySeason["0001"] != 148 || st.Battles.Won != 1 {
		t.Fatalf("winner stats = %+v", st)
	}
	// A second win accumulates into total + season.
	_ = squad.CreditBattle(ctx, win, lose, 100, 50, "0001")
	st, _ = squad.SquadStatsByTeamID(ctx, win)
	if st.CapturePoints.Total != 199 || st.Renown.Total != 198 || st.Battles.Won != 2 {
		t.Errorf("accumulated = %+v", st)
	}
	// Loser gets played counts only, never renown.
	if ls, _ := squad.SquadStatsByTeamID(ctx, lose); ls == nil || ls.Battles.Played != 2 || ls.Renown.Total != 0 {
		t.Errorf("loser stats = %+v", ls)
	}
	// AI/CPU side (non-TM id) is skipped entirely.
	_ = squad.CreditBattle(ctx, win, "BBB9999999999999999", 10, 10, "0001")
	if cpu, _ := squad.SquadStatsByTeamID(ctx, "BBB9999999999999999"); cpu != nil {
		t.Error("CPU side should not get a stats doc")
	}
}

// TestHQFallAndRevivalLive exercises the HQ-fall primitives against a real Mongo: an area cascade to the
// captor, the dissolved-nation flag, and the revival transition. It mirrors the sequence recordHQFall /
// recordRevival drive so the repo-level mechanics are covered without standing up a full messageServer.
func TestHQFallAndRevivalLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live HQ-fall test")
	}
	ctx := context.Background()
	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	world := NewWorldRepository(store)
	_ = world.battlefields.Drop(ctx)
	_ = world.nations.Drop(ctx)
	t.Cleanup(func() { _ = world.battlefields.Drop(ctx); _ = world.nations.Drop(ctx) })

	// Nation B holds its capital (area 2, two battlefields) plus one conquered area (5). A holds area 1.
	seed := []Battlefield{
		{AreaID: 2, MapID: 1, Capacity: 1000, OccB: 1000},
		{AreaID: 2, MapID: 2, Capacity: 1000, OccB: 1000},
		{AreaID: 5, MapID: 1, Capacity: 1000, OccB: 1000},
		{AreaID: 1, MapID: 1, Capacity: 1000, OccA: 1000},
	}
	for _, b := range seed {
		if _, err := world.battlefields.InsertOne(ctx, b); err != nil {
			t.Fatalf("seed bf: %v", err)
		}
	}
	if _, err := world.nations.InsertMany(ctx, []any{NationRecord{CountryCode: "A"}, NationRecord{CountryCode: "B"}}); err != nil {
		t.Fatalf("seed nations: %v", err)
	}

	if owned := ownedSet(t, ctx, world, 'B'); !owned[2] || !owned[5] {
		t.Fatalf("B should own areas 2 and 5 before the fall, got %v", owned)
	}

	// --- HQ fall: A takes area 2 (B's capital) -> capital + cascade flip to A, B dissolved ---
	if err := world.FlipAreaUnlocked(ctx, 2, 'A'); err != nil {
		t.Fatal(err)
	}
	for _, a := range []int32{5} { // cascade B's other areas
		if err := world.FlipAreaUnlocked(ctx, a, 'A'); err != nil {
			t.Fatal(err)
		}
	}
	if err := world.SetNationHQLost(ctx, 'B', 'A'); err != nil {
		t.Fatal(err)
	}
	// Every battlefield of areas 2 and 5 is now 100% A and unlocked.
	for _, areaID := range []byte{2, 5} {
		bfs, _ := world.BattlefieldsByArea(ctx, areaID)
		for _, b := range bfs {
			if b.OccA != b.Capacity || b.OccB != 0 || b.Locked {
				t.Errorf("area %d/%d after fall = %+v, want 100%% A unlocked", areaID, b.MapID, b)
			}
		}
	}
	if captor, _ := world.NationHQLostTo(ctx, 'B'); captor != 'A' {
		t.Errorf("B dissolved captor = %q, want 'A'", captor)
	}
	if owned := ownedSet(t, ctx, world, 'B'); len(owned) != 0 {
		t.Errorf("B should own nothing after the fall, got %v", owned)
	}

	// --- revival: B retakes its capital (area 2) -> flips back, lockout lifts ---
	if err := world.FlipAreaUnlocked(ctx, 2, 'B'); err != nil {
		t.Fatal(err)
	}
	if err := world.ClearNationHQLost(ctx, 'B'); err != nil {
		t.Fatal(err)
	}
	if captor, _ := world.NationHQLostTo(ctx, 'B'); captor != 0 {
		t.Errorf("B should no longer be dissolved, captor = %q", captor)
	}
	if owned := ownedSet(t, ctx, world, 'B'); !owned[2] || owned[5] {
		t.Errorf("after revival B should own only area 2 (not the cascaded 5), got %v", owned)
	}
}

func ownedSet(t *testing.T, ctx context.Context, world *WorldRepository, nation byte) map[int32]bool {
	t.Helper()
	owned, err := world.AreasOwnedBy(ctx, nation)
	if err != nil {
		t.Fatalf("AreasOwnedBy(%c): %v", nation, err)
	}
	set := make(map[int32]bool, len(owned))
	for _, a := range owned {
		set[a] = true
	}
	return set
}
