package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"os"
	"reflect"
	"testing"
)

// seededByArea filters the static seed down to one area's battlefields (as the Mongo query would).
func seededByArea(areaID byte) []Battlefield {
	var out []Battlefield
	for _, b := range seedBattlefields() {
		if byte(b.AreaID) == areaID {
			out = append(out, b)
		}
	}
	return out
}

// TestSeedDerivationMatchesStaticAreaInfo proves Phase 1 is behaviour-preserving for the area-info (197)
// path: deriving wire records from the seeded battlefields equals the static worldData.go output, for
// every area. If this passes, the bytes on the wire are unchanged after the migration.
func TestSeedDerivationMatchesStaticAreaInfo(t *testing.T) {
	for areaID := byte(1); int(areaID) < len(areaBattlefieldCount); areaID++ {
		staticMaps, staticCount := areaBattlefields(areaID)
		maps, count := areaMapRecordsFrom(seededByArea(areaID))
		if count != staticCount {
			t.Errorf("area %d: count = %d, static = %d", areaID, count, staticCount)
		}
		if !reflect.DeepEqual(maps, staticMaps) {
			t.Errorf("area %d: derived area-info records differ from static model", areaID)
		}
	}
}

// TestSeedDerivationMatchesStaticAreaSummary proves the same for the war-map (196) per-area summary.
func TestSeedDerivationMatchesStaticAreaSummary(t *testing.T) {
	for areaID := byte(1); int(areaID) < len(areaBattlefieldCount); areaID++ {
		so, sa, sb, sc := areaControlSummary(areaID)
		o, a, b, c := areaSummaryFrom(seededByArea(areaID))
		if o != so || a != sa || b != sb || c != sc {
			t.Errorf("area %d summary: got (%c,%d,%d,%d), static (%c,%d,%d,%d)", areaID, o, a, b, c, so, sa, sb, sc)
		}
	}
}

// TestSeedNationsRoundTrip proves the nations (195) path: NationRecord <-> NationData round-trips back to
// the static defaults.
func TestSeedNationsRoundTrip(t *testing.T) {
	want := defaultNations()
	recs := seedNations()
	if len(recs) != 3 {
		t.Fatalf("seedNations len = %d, want 3", len(recs))
	}
	for i, r := range recs {
		if got := r.toNationData(); got != want[i] {
			t.Errorf("nation %d round-trip mismatch:\n got  %+v\n want %+v", i, got, want[i])
		}
	}
}

// TestSeedBattlefieldCount sanity-checks the seed against the per-area counts.
func TestSeedBattlefieldCount(t *testing.T) {
	var want int
	for i := 1; i < len(areaBattlefieldCount); i++ {
		want += int(areaBattlefieldCount[i])
	}
	if got := len(seedBattlefields()); got != want {
		t.Errorf("seedBattlefields count = %d, want %d", got, want)
	}
}

// TestRepositorySeedAndReadLive exercises the real Mongo round-trip end-to-end. Skipped unless
// MONGO_TEST_URI is set; it uses a throwaway database and cleans up after itself.
func TestRepositorySeedAndReadLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live world-repository round-trip test")
	}
	ctx := context.Background()

	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewWorldRepository(store)
	// Start clean so the seed runs deterministically.
	_ = repo.battlefields.Drop(ctx)
	_ = repo.nations.Drop(ctx)
	t.Cleanup(func() {
		_ = repo.battlefields.Drop(ctx)
		_ = repo.nations.Drop(ctx)
	})

	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Re-running must be idempotent (no duplicate inserts).
	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema (second run): %v", err)
	}

	// EnsureSchema no longer seeds battlefields (that is the reset tool's job); insert the static layout
	// here so the read-back / derivation round-trip can still be exercised.
	if _, err := repo.battlefields.InsertMany(ctx, toAny(seedBattlefields())); err != nil {
		t.Fatalf("seed battlefields: %v", err)
	}

	bfs, err := repo.BattlefieldsByArea(ctx, 1)
	if err != nil {
		t.Fatalf("BattlefieldsByArea: %v", err)
	}
	maps, count := areaMapRecordsFrom(bfs)
	staticMaps, staticCount := areaBattlefields(1)
	if count != staticCount || !reflect.DeepEqual(maps, staticMaps) {
		t.Errorf("area 1 read back from Mongo differs from static model")
	}

	nations, err := repo.Nations(ctx)
	if err != nil {
		t.Fatalf("Nations: %v", err)
	}
	if len(nations) != 3 {
		t.Fatalf("Nations len = %d, want 3", len(nations))
	}
}

func TestControlAreasFromBattlefields(t *testing.T) {
	// Two battlefields: bf1 (4 occupation points) fully held by A; bf2 (1 point) fully held by B.
	// Total points = 5 -> A = 4/5 = 80% -> round(0.8*22)=18 ; B = 1/5 = 20% -> round(0.2*22)=4 ; C = 0.
	bfs := []Battlefield{
		{Capacity: 100, StrategicValue: 4, OccA: 100, OccB: 0, OccC: 0},
		{Capacity: 100, StrategicValue: 1, OccA: 0, OccB: 100, OccC: 0},
	}
	a, b, c := controlAreasFromBattlefields(bfs)
	if a != 18 || b != 4 || c != 0 {
		t.Errorf("control areas = (%d,%d,%d), want (18,4,0)", a, b, c)
	}

	// A battlefield split 60/40 (A/C) worth 2 points, plus a zero-capacity battlefield that must be
	// skipped without dividing by zero.
	bfs2 := []Battlefield{
		{Capacity: 100, StrategicValue: 2, OccA: 60, OccB: 0, OccC: 40},
		{Capacity: 0, StrategicValue: 9, OccA: 5, OccB: 5, OccC: 5}, // skipped (no capacity)
	}
	a2, b2, c2 := controlAreasFromBattlefields(bfs2)
	// A = 60% -> round(0.6*22)=13 ; C = 40% -> round(0.4*22)=9 ; B = 0.
	if a2 != 13 || b2 != 0 || c2 != 9 {
		t.Errorf("control areas (split) = (%d,%d,%d), want (13,0,9)", a2, b2, c2)
	}

	// No occupation points anywhere -> all zero, no panic.
	if a3, b3, c3 := controlAreasFromBattlefields(nil); a3 != 0 || b3 != 0 || c3 != 0 {
		t.Errorf("empty = (%d,%d,%d), want (0,0,0)", a3, b3, c3)
	}
}

// TestCreditNationDonationLive covers the donation credit (+status bytes) against real Mongo. Skipped
// without MONGO_TEST_URI.
func TestCreditNationDonationLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live donation-credit test")
	}
	ctx := context.Background()

	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewWorldRepository(store)
	_ = repo.nations.Drop(ctx)
	t.Cleanup(func() { _ = repo.nations.Drop(ctx) })
	if err := repo.EnsureSchema(ctx); err != nil { // seeds nations A/B/C
		t.Fatalf("EnsureSchema: %v", err)
	}

	incomeOf := func(code byte) int32 {
		recs, err := repo.Nations(ctx)
		if err != nil {
			t.Fatalf("Nations: %v", err)
		}
		for _, r := range recs {
			if len(r.CountryCode) > 0 && r.CountryCode[0] == code {
				return r.TotalIncome
			}
		}
		t.Fatalf("nation %q not found", string(code))
		return 0
	}

	before := incomeOf('A')
	if st, err := repo.CreditNationDonation(ctx, 'A', 10_000_000); err != nil || st != '1' {
		t.Fatalf("credit A = (%q,%v), want ('1',nil)", string(st), err)
	}
	if got := incomeOf('A'); got != before+10_000_000 {
		t.Errorf("A totalIncome = %d, want %d", got, before+10_000_000)
	}

	// Unknown nation -> '3', no side effects.
	if st, _ := repo.CreditNationDonation(ctx, 'Z', 10_000_000); st != '3' {
		t.Errorf("credit unknown = %q, want '3'", string(st))
	}

	// A dead nation -> '2' and no credit.
	if _, err := repo.nations.UpdateOne(ctx,
		map[string]any{"countryCode": "B"},
		map[string]any{"$set": map[string]any{"deadFlag": int32(1)}}); err != nil {
		t.Fatalf("kill B: %v", err)
	}
	beforeB := incomeOf('B')
	if st, _ := repo.CreditNationDonation(ctx, 'B', 10_000_000); st != '2' {
		t.Errorf("credit dead B = %q, want '2'", string(st))
	}
	if got := incomeOf('B'); got != beforeB {
		t.Errorf("dead B totalIncome changed: %d -> %d", beforeB, got)
	}
}

// TestEventsAndAreaCountsLive covers the world-event log ordering and per-nation area control against real
// Mongo. Skipped without MONGO_TEST_URI.
func TestEventsAndAreaCountsLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live events/area-counts test")
	}
	ctx := context.Background()

	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewWorldRepository(store)
	_ = repo.events.Drop(ctx)
	_ = repo.battlefields.Drop(ctx)
	t.Cleanup(func() { _ = repo.events.Drop(ctx); _ = repo.battlefields.Drop(ctx) })

	// RecentEvents returns newest-first by createdAt.
	_ = repo.RecordEvent(ctx, EventRecord{CreatedAt: 100, TemplateID: 1})
	_ = repo.RecordEvent(ctx, EventRecord{CreatedAt: 300, TemplateID: 3})
	_ = repo.RecordEvent(ctx, EventRecord{CreatedAt: 200, TemplateID: 2})
	evs, err := repo.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(evs) != 3 || evs[0].TemplateID != 3 || evs[1].TemplateID != 2 || evs[2].TemplateID != 1 {
		t.Errorf("RecentEvents order = %+v, want templates 3,2,1", evs)
	}

	// NationAreaCounts: area 1 fully A, area 2 fully B -> A:1 B:1 C:0.
	if _, err := repo.battlefields.InsertMany(ctx, []any{
		Battlefield{AreaID: 1, MapID: 1, Capacity: 100, StrategicValue: 1, OccA: 100},
		Battlefield{AreaID: 2, MapID: 1, Capacity: 100, StrategicValue: 1, OccB: 100},
	}); err != nil {
		t.Fatalf("seed battlefields: %v", err)
	}
	counts, err := repo.NationAreaCounts(ctx)
	if err != nil {
		t.Fatalf("NationAreaCounts: %v", err)
	}
	if counts['A'] != 1 || counts['B'] != 1 || counts['C'] != 0 {
		t.Errorf("area counts = %v, want A:1 B:1 C:0", counts)
	}
}

// TestCaptureLedgerLive covers the per-squad capture-points ledger (backs |s2=) against real Mongo.
func TestCaptureLedgerLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live capture-ledger test")
	}
	ctx := context.Background()

	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewWorldRepository(store)
	_ = repo.captures.Drop(ctx)
	t.Cleanup(func() { _ = repo.captures.Drop(ctx) })

	// Area 5: Alpha earns 100 (map1) + 50 (map2) = 150; Beta earns 200 (map1).
	_ = repo.CreditCapture(ctx, 5, 1, "Alpha", 100)
	_ = repo.CreditCapture(ctx, 5, 2, "Alpha", 50)
	_ = repo.CreditCapture(ctx, 5, 1, "Beta", 200)

	// Battlefield 5/1 top = Beta (200 > 100).
	if s, _ := repo.TopSquadForBattlefield(ctx, 5, 1); s != "Beta" {
		t.Errorf("battlefield 5/1 top = %q, want Beta", s)
	}
	// Battlefield 5/2 top = Alpha (only contributor).
	if s, _ := repo.TopSquadForBattlefield(ctx, 5, 2); s != "Alpha" {
		t.Errorf("battlefield 5/2 top = %q, want Alpha", s)
	}
	// Region 5 total: Beta 200 > Alpha 150.
	if s, _ := repo.TopSquadForRegion(ctx, 5); s != "Beta" {
		t.Errorf("region 5 top = %q, want Beta", s)
	}
	// Unknown battlefield -> empty.
	if s, _ := repo.TopSquadForBattlefield(ctx, 9, 9); s != "" {
		t.Errorf("unknown battlefield top = %q, want empty", s)
	}
}
