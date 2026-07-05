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

// TestBattleReportIngestLive exercises the occupation move + squad-stat accumulation against a real Mongo.
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
	_ = squad.stats.Drop(ctx)
	t.Cleanup(func() { _ = world.battlefields.Drop(ctx); _ = squad.stats.Drop(ctx) })

	// --- occupation move ---
	// Seed a battlefield fully held by nation C. Nation A wins -> A gains 99, C loses 99.
	if _, err := world.battlefields.InsertOne(ctx, Battlefield{AreaID: 20, MapID: 2, Capacity: 20000, OccC: 20000}); err != nil {
		t.Fatalf("seed bf: %v", err)
	}
	if err := world.ApplyBattleOccupation(ctx, 20, 2, 'A', 'C', 99); err != nil {
		t.Fatalf("ApplyBattleOccupation: %v", err)
	}
	if bf, _ := world.BattlefieldByAreaMap(ctx, 20, 2); bf == nil || bf.OccA != 99 || bf.OccC != 20000-99 {
		t.Errorf("occupation = %+v, want occA=99 occC=%d", bf, 20000-99)
	}

	// Clamp: near-boundary battlefield; winner caps at capacity, loser floors at 0.
	if _, err := world.battlefields.InsertOne(ctx, Battlefield{AreaID: 1, MapID: 1, Capacity: 100, OccA: 90, OccC: 10}); err != nil {
		t.Fatalf("seed bf2: %v", err)
	}
	if err := world.ApplyBattleOccupation(ctx, 1, 1, 'A', 'C', 50); err != nil {
		t.Fatal(err)
	}
	if bf, _ := world.BattlefieldByAreaMap(ctx, 1, 1); bf == nil || bf.OccA != 100 || bf.OccC != 0 {
		t.Errorf("clamp = %+v, want occA=100 occC=0", bf)
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
