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

	// Snapshot 11/4's original occupation so the shared test DB is left exactly as we found it (this test
	// deploys a weapon there, which rewrites occupation as durability).
	orig, err := repo.BattlefieldByAreaMap(ctx, 11, 4)
	if err != nil || orig == nil {
		t.Skipf("battlefield 11/4 not available (%v); skipping", err)
	}
	origA, origB, origC := orig.OccA, orig.OccB, orig.OccC

	// Clean slate for nation A's weapon (deployment + any leftover weapon news), and restore both the weapon
	// state and the original occupation at the end.
	cleanup := func() {
		bg := context.Background()
		_, _ = repo.ClearWeaponDeployed(bg, nation)
		site, _ := UnidentifiedWeaponSiteFor(nation)
		rows := make([]int32, 0, len(site.Rows))
		for _, r := range site.Rows {
			rows = append(rows, r)
		}
		_, _ = repo.events.DeleteMany(bg, map[string]any{"templateId": map[string]any{"$in": rows}})
		_, _ = repo.battlefields.UpdateOne(bg,
			map[string]any{"areaId": int32(11), "mapId": int32(4)},
			map[string]any{"$set": map[string]any{"occA": origA, "occB": origB, "occC": origC}})
	}
	cleanup()
	defer cleanup()

	// Deploy: withstands 2 successful missions. Deployment sets the site to A's 100% control (full bar).
	if _, err := repo.SetWeaponDeployed(ctx, nation, 2); err != nil {
		t.Fatalf("SetWeaponDeployed: %v (is battlefield 11/4 seeded?)", err)
	}
	if d, err := repo.DeployedWeaponNations(ctx); err != nil || !d[nation] {
		t.Fatalf("after deploy: DeployedWeaponNations=%v err=%v, want A deployed", d, err)
	}
	bf, _ := repo.BattlefieldByAreaMap(ctx, 11, 4)
	capacity := bf.Capacity
	if bf.OccA != capacity {
		t.Fatalf("after deploy, durability should be full: occA=%d want capacity=%d", bf.OccA, capacity)
	}

	applier := NewBattleApplier(repo, nil, true, 1, "WTEST")
	// A weapon-destruction report: area 99, the weapon (A) is the loser, attacker (B) wins.
	weaponReport := BattleResult{
		AreaID: WeaponBattleAreaID, MapID: 4,
		WinnerNation: 'B', LoserNation: nation,
		WinnerTeam: "TM0000000000000001", LoserTeam: "AA9999999999999999",
		OccDelta: 100, WinnerMerit: 100,
	}

	// Hit 1 of 2 -> still deployed, durability drained to ~half (occupation = capacity*(2-1)/2).
	applier.Apply(ctx, weaponReport)
	if d, _ := repo.DeployedWeaponNations(ctx); !d[nation] {
		t.Fatalf("after hit 1/2 the weapon should still be deployed")
	}
	bf, _ = repo.BattlefieldByAreaMap(ctx, 11, 4)
	if want := capacity / 2; bf.OccA != want {
		t.Fatalf("after hit 1/2 durability should be half: occA=%d want %d", bf.OccA, want)
	}

	// Hit 2 of 2 -> destroyed: deployment cleared, destroyed news filed, and the site restored to A's 100%.
	applier.Apply(ctx, weaponReport)
	if d, _ := repo.DeployedWeaponNations(ctx); d[nation] {
		t.Fatalf("after hit 2/2 the weapon should be destroyed (deployment cleared)")
	}
	bf, _ = repo.BattlefieldByAreaMap(ctx, 11, 4)
	if bf.OccA != capacity || bf.OccB != 0 || bf.OccC != 0 {
		t.Fatalf("after destruction the site should be A's 100%% default: occA=%d occB=%d occC=%d (capacity=%d)", bf.OccA, bf.OccB, bf.OccC, capacity)
	}
	if bf.WeaponMissionsToDestroy != 0 || bf.WeaponHits != 0 {
		t.Fatalf("after destruction weapon counters should be cleared: threshold=%d hits=%d", bf.WeaponMissionsToDestroy, bf.WeaponHits)
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
