package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestPlayersByNationFunctional exercises the active-players-by-nation aggregation against a real Mongo
// (guarded by MONGO_URI). It seeds a faction-C squad + a connected player in that squad and a squad-less
// connected player, then checks both metrics.
func TestPlayersByNationFunctional(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("set MONGO_URI to run the players-by-nation functional test")
	}
	db := os.Getenv("MONGO_DATABASE")
	if db == "" {
		db = "test_pbn_probe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, uri, db)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewSquadRepository(store)
	sq, err := repo.CreateSquad(ctx, "PBN_PROBE", "C", CombasProfile{XUID: "PBN_X1", UserID: "PBN_U1", Gamertag: "lead"})
	if err != nil {
		t.Fatalf("seed squad: %v", err)
	}
	// A connected player in that squad, and one with no squad.
	_, _ = repo.players.InsertMany(ctx, []any{
		bson.M{"xuid": "PBN_X1", "gamertag": "lead", "state": 19},
		bson.M{"xuid": "PBN_LONER", "gamertag": "loner", "state": 19},
	})
	defer func() {
		_, _ = repo.squads.DeleteOne(ctx, bson.M{"teamId": sq.TeamID})
		_, _ = repo.profiles.DeleteMany(ctx, bson.M{"teamId": sq.TeamID})
		_, _ = repo.players.DeleteMany(ctx, bson.M{"xuid": bson.M{"$in": bson.A{"PBN_X1", "PBN_LONER"}}})
	}()

	got, err := repo.PlayersByNation(ctx)
	if err != nil {
		t.Fatalf("PlayersByNation: %v", err)
	}
	if got.Registered["C"] < 1 {
		t.Errorf("registered C = %d, want >=1 (the seeded leader)", got.Registered["C"])
	}
	if got.Online["C"] < 1 {
		t.Errorf("online C = %d, want >=1 (the connected leader)", got.Online["C"])
	}
	if got.Online["none"] < 1 {
		t.Errorf("online none = %d, want >=1 (the squad-less connected player)", got.Online["none"])
	}
}
