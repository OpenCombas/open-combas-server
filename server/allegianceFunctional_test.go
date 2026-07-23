package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"os"
	"testing"
	"time"
)

// TestChangeAllegianceFunctional exercises the msgCode-201 repo logic against a real Mongo (guarded by
// MONGO_URI so `go test ./...` stays offline). It seeds a squad, then checks the three outcomes the client's
// parser distinguishes: change -> '1', repeat -> '2' (same state), unknown squad / bad nation -> error.
func TestChangeAllegianceFunctional(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("set MONGO_URI to run the allegiance functional test")
	}
	db := os.Getenv("MONGO_DATABASE")
	if db == "" {
		db = "test_allegiance_probe"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, uri, db)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewSquadRepository(store)
	leader := CombasProfile{XUID: "AX1", UserID: "AU1", Gamertag: "leader"}
	sq, err := repo.CreateSquad(ctx, "ALLEGIANCE_PROBE", "A", leader) // starts as nation A (Tarakia)
	if err != nil {
		t.Fatalf("seed squad: %v", err)
	}
	// Clean up the squad + the leader's profile link so the probe leaves no residue.
	defer func() {
		_, _ = repo.squads.DeleteOne(ctx, map[string]any{"teamId": sq.TeamID})
		_, _ = repo.profiles.DeleteMany(ctx, map[string]any{"teamId": sq.TeamID})
	}()

	// A -> C is a real change: Complete.
	if st, err := repo.ChangeAllegiance(ctx, sq.TeamID, 'C'); err != nil || st != allegianceComplete {
		t.Fatalf("A->C: got (%q, %v), want ('1', nil)", string(st), err)
	}
	if got, _ := repo.SquadByTeamID(ctx, sq.TeamID); got == nil || got.Faction != "C" {
		t.Fatalf("faction after change = %q, want C", faction(got))
	}

	// C -> C is a no-op: Demand Same As Current State.
	if st, err := repo.ChangeAllegiance(ctx, sq.TeamID, 'C'); err != nil || st != allegianceSameState {
		t.Fatalf("C->C: got (%q, %v), want ('2', nil)", string(st), err)
	}

	// Unknown squad -> error status, no server error.
	if st, err := repo.ChangeAllegiance(ctx, "TM_NOPE", 'B'); err != nil || st != allegianceError {
		t.Fatalf("unknown squad: got (%q, %v), want ('9', nil)", string(st), err)
	}

	// Out-of-range nation -> error status.
	if st, _ := repo.ChangeAllegiance(ctx, sq.TeamID, 'Z'); st != allegianceError {
		t.Fatalf("bad nation: got %q, want '9'", string(st))
	}
}

func faction(s *Squad) string {
	if s == nil {
		return "<nil>"
	}
	return s.Faction
}
