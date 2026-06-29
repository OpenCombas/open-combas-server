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
