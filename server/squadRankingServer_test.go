package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"encoding/binary"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func mustInsert(t *testing.T, ctx context.Context, coll *mongo.Collection, doc any) {
	t.Helper()
	if _, err := coll.InsertOne(ctx, doc); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func rankingPacket(body string) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+len(body)+1)
	if _, err := binary.Encode(pkt, binary.LittleEndian, CreateHeader(squadXuid, [8]byte{})); err != nil {
		panic(err)
	}
	copy(pkt[constants.MinHelloMessageSize:], body)
	return pkt
}

func TestParseSquadRanking(t *testing.T) {
	teamID, kbn, useSeason, ok := parseSquadRanking(rankingPacket("ibac2,TM0001000000000005,2,3"))
	if !ok || teamID != "TM0001000000000005" || kbn != 3 || !useSeason {
		t.Errorf("parse = (%q,%d,%v,%v)", teamID, kbn, useSeason, ok)
	}
	// SEA=1 => Total (useSeason false); KBN=1 Renown.
	if _, kbn, useSeason, ok := parseSquadRanking(rankingPacket("ibac2,TM5,1,1")); !ok || kbn != 1 || useSeason {
		t.Errorf("total-renown parse = (%d,%v,%v)", kbn, useSeason, ok)
	}
	// bad KBN rejected.
	if _, _, _, ok := parseSquadRanking(rankingPacket("ibac2,TM5,1,9")); ok {
		t.Error("kbn=9 should be rejected")
	}
}

func TestWriteRankBlock(t *testing.T) {
	block := make([]byte, rankingBlockSize)
	entries := []RankEntry{{TeamID: "TM1", Name: "Alpha", Value: 100}, {TeamID: "TM2", Name: "Bravo", Value: 50}}
	writeRankBlock(block, entries)

	if got := binary.LittleEndian.Uint32(block[0:]); got != 0 { // status: 0 = success
		t.Errorf("status = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(block[4:]); got != 2 { // count
		t.Errorf("count = %d, want 2", got)
	}
	if string(block[rankingSeasonOffset:rankingSeasonOffset+4]) != currentSeason {
		t.Errorf("season code = %q, want %q", block[rankingSeasonOffset:rankingSeasonOffset+4], currentSeason)
	}
	// name + value at rank 0 and rank 1.
	if name0 := trimNullString(block[rankingNameOffset : rankingNameOffset+rankingNameStride]); name0 != "Alpha" {
		t.Errorf("name0 = %q", name0)
	}
	if v0 := binary.LittleEndian.Uint32(block[rankingValueOffset:]); v0 != 100 {
		t.Errorf("value0 = %d, want 100", v0)
	}
	if name1 := trimNullString(block[rankingNameOffset+rankingNameStride : rankingNameOffset+2*rankingNameStride]); name1 != "Bravo" {
		t.Errorf("name1 = %q", name1)
	}
	if v1 := binary.LittleEndian.Uint32(block[rankingValueOffset+4:]); v1 != 50 {
		t.Errorf("value1 = %d, want 50", v1)
	}
	// the last name slot ends exactly where the value array begins (layout invariant).
	if rankingNameOffset+rankingMaxEntries*rankingNameStride != rankingValueOffset {
		t.Errorf("name section [%d,%d) does not abut value array at %d", rankingNameOffset, rankingNameOffset+rankingMaxEntries*rankingNameStride, rankingValueOffset)
	}
}

// TestRankSquadsLive seeds squads + stats and checks ordering/values per stat & season against Mongo.
func TestRankSquadsLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live ranking test")
	}
	ctx := context.Background()
	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewSquadRepository(store)
	_ = repo.squads.Drop(ctx)
	_ = repo.stats.Drop(ctx)
	t.Cleanup(func() { _ = repo.squads.Drop(ctx); _ = repo.stats.Drop(ctx) })

	// Two squads: A (2 members, big renown), B (1 member, less renown but higher per-member).
	mustInsert(t, ctx, repo.squads, Squad{TeamID: "TMA", Name: "Alpha", Members: []SquadMemberRecord{{}, {}}})
	mustInsert(t, ctx, repo.squads, Squad{TeamID: "TMB", Name: "Bravo", Members: []SquadMemberRecord{{}}})
	// A: renown 300 (season 100), capture 90. B: renown 200 (season 200), capture 40.
	_ = repo.CreditBattle(ctx, "TMA", "TMB", 90, 200, "0000") // old season
	_ = repo.CreditBattle(ctx, "TMA", "TMB", 0, 100, currentSeason)
	_ = repo.CreditBattle(ctx, "TMB", "TMA", 40, 200, currentSeason)

	// Renown, Total: A(300) > B(200).
	if e, _ := repo.RankSquads(ctx, 1, false, currentSeason); e[0].TeamID != "TMA" || e[0].Value != 300 || e[1].Value != 200 {
		t.Errorf("renown total = %+v", e)
	}
	// Renown, Season: B(200) > A(100).
	if e, _ := repo.RankSquads(ctx, 1, true, currentSeason); e[0].TeamID != "TMB" || e[0].Value != 200 || e[1].Value != 100 {
		t.Errorf("renown season = %+v", e)
	}
	// Capture Points, Total: A(90) > B(40).
	if e, _ := repo.RankSquads(ctx, 2, false, currentSeason); e[0].TeamID != "TMA" || e[0].Value != 90 {
		t.Errorf("capture total = %+v", e)
	}
	// Renown-Per-Member, Total: A 300/2=150, B 200/1=200 -> B first.
	if e, _ := repo.RankSquads(ctx, 3, false, currentSeason); e[0].TeamID != "TMB" || e[0].Value != 200 || e[1].Value != 150 {
		t.Errorf("rpm total = %+v", e)
	}
}
