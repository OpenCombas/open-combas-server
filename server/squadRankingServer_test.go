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
	entries := []RankEntry{
		{TeamID: "TM1", Name: "Alpha", Value: 100, Nation: 'A', Grade: 7},
		{TeamID: "TM2", Name: "Bravo", Value: 50, Nation: 'C', Grade: 2},
	}
	writeRankBlock(block, entries)

	if got := block[rankingStatusOffset]; got != 0 {
		t.Errorf("status = %d, want 0", got)
	}
	if got := block[rankingCountOffset]; got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	for i, want := range entries {
		if got := block[rankingNationOffset+i]; got != want.Nation {
			t.Errorf("nation[%d] = %q, want %q", i, got, want.Nation)
		}
		if got := block[rankingGradeOffset+i]; got != want.Grade {
			t.Errorf("grade[%d] = %d, want %d", i, got, want.Grade)
		}
		off := rankingNameOffset + i*rankingNameStride
		if got := trimNullString(block[off : off+rankingNameStride]); got != want.Name {
			t.Errorf("name[%d] = %q, want %q", i, got, want.Name)
		}
		if got := binary.LittleEndian.Uint32(block[rankingValueOffset+i*4:]); got != uint32(want.Value) {
			t.Errorf("value[%d] = %d, want %d", i, got, want.Value)
		}
	}
}

// A 16-byte slot read as a C string must always keep its terminator, or the render runs one name into the
// next.
func TestWriteRankBlockTruncatesLongNames(t *testing.T) {
	block := make([]byte, rankingBlockSize)
	long := "ABCDEFGHIJKLMNOPQRSTUVWXYZ" // 26 chars, far over the slot
	writeRankBlock(block, []RankEntry{{Name: long, Nation: 'B', Grade: 1}})

	slot := block[rankingNameOffset : rankingNameOffset+rankingNameStride]
	if slot[rankingNameStride-1] != 0 {
		t.Errorf("name slot has no NUL terminator: %q", slot)
	}
	if got := trimNullString(slot); got != long[:rankingNameStride-1] {
		t.Errorf("truncated name = %q, want %q", got, long[:rankingNameStride-1])
	}
}

// The requester's own standing lives in BLOCK 0's header only, and must carry the true global rank -- the
// client renders it precisely when that rank exceeds the 100 entries it was sent.
func TestWriteOwnStanding(t *testing.T) {
	block := make([]byte, rankingBlockSize)
	writeOwnStanding(block, 137, RankEntry{Value: 4321, Grade: 5})

	if got := binary.LittleEndian.Uint32(block[rankingOwnRankOffset:]); got != 137 {
		t.Errorf("own rank = %d, want 137", got)
	}
	if got := binary.LittleEndian.Uint32(block[rankingOwnValueOffset:]); got != 4321 {
		t.Errorf("own value = %d, want 4321", got)
	}
	if got := block[rankingOwnGradeOffset]; got != 5 {
		t.Errorf("own grade = %d, want 5", got)
	}

	// An unranked requester leaves the header zeroed rather than claiming rank 0.
	zero := make([]byte, rankingBlockSize)
	writeOwnStanding(zero, 0, RankEntry{Value: 999, Grade: 9})
	for i := 0; i < rankingCountOffset; i++ {
		if zero[i] != 0 {
			t.Fatalf("unranked requester wrote header byte %d = %d", i, zero[i])
		}
	}
}

// The layout must account for all 1120 bytes with no gap and no overlap. This is the invariant that
// distinguishes the corrected layout from the previous (wrong) one, so assert it directly.
func TestRankBlockLayoutCloses(t *testing.T) {
	checks := []struct {
		name       string
		start, end int
	}{
		{"nation array ends where names begin", rankingNationOffset + rankingMaxEntries, rankingNameOffset},
		{"name array ends where grades begin", rankingNameOffset + rankingMaxEntries*rankingNameStride, rankingGradeOffset},
		{"value array ends where status begins", rankingValueOffset + rankingMaxEntries*4, rankingStatusOffset},
	}
	for _, c := range checks {
		if c.start != c.end {
			t.Errorf("%s: %d != %d", c.name, c.start, c.end)
		}
	}
	if rankingGradeOffset+rankingMaxEntries > rankingValueOffset {
		t.Errorf("grade array overruns the value array (%d > %d)", rankingGradeOffset+rankingMaxEntries, rankingValueOffset)
	}
	if rankingStatusOffset >= rankingBlockSize {
		t.Errorf("status byte %d is outside the %d-byte block", rankingStatusOffset, rankingBlockSize)
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

// Blocks are ranks 1-50 / 51-100 of one list, so block 1 stays empty until block 0 is full, and anything
// past 100 is dropped (the client is hard-capped at 100 and the request has no offset).
func TestPaginateRanking(t *testing.T) {
	mk := func(n int) []RankEntry {
		e := make([]RankEntry, n)
		for i := range e {
			e[i] = RankEntry{Value: int32(1000 - i)}
		}
		return e
	}
	cases := []struct {
		n            int
		want0, want1 int
	}{
		{0, 0, 0},
		{1, 1, 0},
		{49, 49, 0},
		{50, 50, 0},   // block 0 exactly full, block 1 still empty
		{51, 50, 1},   // spillover begins
		{100, 50, 50}, // both full
		{137, 50, 50}, // excess dropped
	}
	for _, tc := range cases {
		first, second := paginateRanking(mk(tc.n))
		if len(first) != tc.want0 || len(second) != tc.want1 {
			t.Errorf("n=%d -> (%d,%d), want (%d,%d)", tc.n, len(first), len(second), tc.want0, tc.want1)
		}
	}

	// The seam must be contiguous: block 1's first entry is the immediate successor of block 0's last.
	first, second := paginateRanking(mk(60))
	if first[len(first)-1].Value != 1000-49 || second[0].Value != 1000-50 {
		t.Errorf("seam broken: block0 last = %d, block1 first = %d", first[len(first)-1].Value, second[0].Value)
	}
}
