package server

import (
	"context"
	"os"
	"testing"
	"time"

	"ChromehoundsStatusServer/persistence"
)

// TestWeaponHitLoopIntegration drives the whole area-99 weapon-attrition loop against a real MongoDB:
// deploy with a mission threshold, feed successful destruction reports, and confirm the weapon auto-destroys
// (deployment cleared + "destroyed" news) exactly on the threshold. Guarded by MONGO_URI so `go test` skips
// it without a database. Uses nation A (Wakool 11/4) and fully cleans up after itself.
func TestWeaponHitLoopIntegration(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("set MONGO_URI to run the weapon-hit integration test")
	}
	db := os.Getenv("MONGO_DATABASE")
	if db == "" {
		db = "test"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, uri, db)
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	repo := NewWorldRepository(store)
	const nation = byte('A')

	// Clean slate for nation A's weapon (deployment + any leftover weapon news), and restore it at the end
	// so a shared test DB is left as we found it.
	cleanup := func() {
		_, _ = repo.ClearWeaponDeployed(context.Background(), nation)
		site, _ := UnidentifiedWeaponSiteFor(nation)
		rows := make([]int32, 0, len(site.Rows))
		for _, r := range site.Rows {
			rows = append(rows, r)
		}
		_, _ = repo.events.DeleteMany(context.Background(), map[string]any{"templateId": map[string]any{"$in": rows}})
	}
	cleanup()
	defer cleanup()

	// Deploy: withstands 2 successful missions.
	if _, err := repo.SetWeaponDeployed(ctx, nation, 2); err != nil {
		t.Fatalf("SetWeaponDeployed: %v (is battlefield 11/4 seeded?)", err)
	}
	if d, err := repo.DeployedWeaponNations(ctx); err != nil || !d[nation] {
		t.Fatalf("after deploy: DeployedWeaponNations=%v err=%v, want A deployed", d, err)
	}

	applier := NewBattleApplier(repo, nil, true, 1, "WTEST")
	// A weapon-destruction report: area 99, the weapon (A) is the loser, attacker (B) wins.
	weaponReport := BattleResult{
		AreaID: WeaponBattleAreaID, MapID: 4,
		WinnerNation: 'B', LoserNation: nation,
		WinnerTeam: "TM0000000000000001", LoserTeam: "AA9999999999999999",
		OccDelta: 100, WinnerMerit: 100,
	}

	// Hit 1 of 2 -> still deployed.
	applier.Apply(ctx, weaponReport)
	if d, _ := repo.DeployedWeaponNations(ctx); !d[nation] {
		t.Fatalf("after hit 1/2 the weapon should still be deployed")
	}

	// Hit 2 of 2 -> destroyed: deployment cleared and the destroyed news filed.
	applier.Apply(ctx, weaponReport)
	if d, _ := repo.DeployedWeaponNations(ctx); d[nation] {
		t.Fatalf("after hit 2/2 the weapon should be destroyed (deployment cleared)")
	}
	state, err := repo.CurrentWeaponPhase(ctx, nation, 50)
	if err != nil {
		t.Fatalf("CurrentWeaponPhase: %v", err)
	}
	if !state.Found || state.Active || state.Last != WeaponDestroyed {
		t.Fatalf("expected last weapon phase = destroyed and inactive, got %+v", state)
	}

	// A further report for a now-undeployed weapon must be a harmless no-op (no panic, stays destroyed).
	applier.Apply(ctx, weaponReport)
	if d, _ := repo.DeployedWeaponNations(ctx); d[nation] {
		t.Fatalf("a stray weapon report must not redeploy the weapon")
	}
}
