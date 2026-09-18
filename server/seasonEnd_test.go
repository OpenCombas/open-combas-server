package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"os"
	"testing"
	"time"
)

// --- pure logic (no Mongo) --------------------------------------------------

func TestDetermineOutcome(t *testing.T) {
	nat := func(code string, dead int32, lostTo string) NationRecord {
		return NationRecord{CountryCode: code, DeadFlag: dead, HQLostTo: lostTo}
	}
	cases := []struct {
		name     string
		nations  []NationRecord
		wantKind string
		wantVic  byte
	}{
		{"all three standing", []NationRecord{nat("A", 0, ""), nat("B", 0, ""), nat("C", 0, "")}, "truce3", 0},
		{"one eliminated -> 2-way truce", []NationRecord{nat("A", 0, ""), nat("B", 1, ""), nat("C", 0, "")}, "truce2", 0},
		{"one dissolved (HQ lost) counts as fallen", []NationRecord{nat("A", 0, ""), nat("B", 0, "A"), nat("C", 0, "")}, "truce2", 0},
		{"two fallen -> victory C", []NationRecord{nat("A", 1, ""), nat("B", 0, "C"), nat("C", 0, "")}, "victory", 'C'},
		{"only A stands -> victory A", []NationRecord{nat("A", 0, ""), nat("B", 1, ""), nat("C", 1, "")}, "victory", 'A'},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, vic := DetermineOutcome(c.nations)
			if kind != c.wantKind || vic != c.wantVic {
				t.Fatalf("DetermineOutcome = (%q, %q), want (%q, %q)", kind, string(vic), c.wantKind, string(c.wantVic))
			}
		})
	}
}

func TestSeasonEndEventRow(t *testing.T) {
	cases := []struct {
		kind    string
		victor  byte
		wantRow int32
		wantOK  bool
	}{
		{"victory", 'A', 10, true},
		{"victory", 'B', 12, true},
		{"victory", 'C', 14, true},
		{"victory", 'Z', 0, false}, // unknown nation
		{"truce2", 0, peaceAccordRow, true},
		{"truce3", 0, peaceAccordRow, true},
		{"nonsense", 0, 0, false},
	}
	for _, c := range cases {
		row, ok := SeasonEndEventRow(c.kind, c.victor)
		if row != c.wantRow || ok != c.wantOK {
			t.Errorf("SeasonEndEventRow(%q,%q) = (%d,%v), want (%d,%v)", c.kind, string(c.victor), row, ok, c.wantRow, c.wantOK)
		}
	}
}

func TestSeasonEndSchedulePending(t *testing.T) {
	if (*SeasonEndSchedule)(nil).Pending() {
		t.Error("nil schedule must not be pending")
	}
	if !(&SeasonEndSchedule{AppliedAt: 0}).Pending() {
		t.Error("appliedAt=0 must be pending")
	}
	if (&SeasonEndSchedule{AppliedAt: 123}).Pending() {
		t.Error("applied schedule must not be pending")
	}
}

// --- live Mongo round-trip (gated on MONGO_TEST_URI) ------------------------

func TestSeasonEndApplyLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live season-end applier test")
	}
	ctx := context.Background()
	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewWorldRepository(store)
	clean := func() {
		for _, c := range []string{battlefieldsCollection, nationsCollection, eventsCollection,
			capturesCollection, areaStatsCollection, maintenanceCollection, serverConfigCollection} {
			_ = store.Collection(c).Drop(ctx)
		}
	}
	clean()
	// Don't leak the mutated season globals into other tests.
	t.Cleanup(func() { clean(); ApplySeasonStart(0); ApplySeasonNumber(defaultSeasonNumber) })

	if err := repo.EnsureSchema(ctx); err != nil { // seeds the 3 default (alive) nations
		t.Fatalf("EnsureSchema: %v", err)
	}
	if _, err := repo.battlefields.InsertMany(ctx, toAny(seedBattlefields())); err != nil {
		t.Fatalf("seed battlefields: %v", err)
	}
	if err := SaveSeasonNumber(ctx, store, 14); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	ApplySeasonNumber(14)
	ApplySeasonStart(0)

	now := time.Now()
	t1 := now.Add(time.Hour) // maintenance window still in the future -> phase 2 not yet due
	t2 := now.Add(2 * time.Hour)
	sched := &SeasonEndSchedule{
		CreatedAt: now.Unix(), EndsAt: now.Add(-time.Minute).Unix(), // t0 already passed -> phase 1 due
		WindowStart: t1.Unix(), WindowEnd: t2.Unix(),
		FromSeason: 14, NextSeason: 15, Downscale: 1,
	}
	if err := SaveSeasonEnd(ctx, store, sched); err != nil {
		t.Fatalf("SaveSeasonEnd: %v", err)
	}

	// Phase 1 only.
	if err := ApplyDueSeasonEnd(ctx, store, repo, now); err != nil {
		t.Fatalf("apply (phase 1): %v", err)
	}
	got, _ := LoadSeasonEnd(ctx, store)
	if got.LockedAt == 0 {
		t.Fatal("phase 1 did not stamp lockedAt")
	}
	if got.AppliedAt != 0 {
		t.Fatal("phase 2 ran too early (windowStart is in the future)")
	}
	if got.Outcome != "truce3" { // three default nations alive
		t.Errorf("outcome = %q, want truce3", got.Outcome)
	}
	if err := RefreshSeasonState(ctx, store); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !SeasonLocked() {
		t.Error("maps should be locked after phase 1 (seasonStartsAt = t2 in the future)")
	}
	if w, err := repo.GetMaintenanceWindow(ctx); err != nil || w == nil || w.Start.Unix() != t1.Unix() || w.End.Unix() != t2.Unix() {
		t.Errorf("maintenance window = %v (err %v), want [%d,%d]", w, err, t1.Unix(), t2.Unix())
	}
	if evs, _ := repo.RecentEvents(ctx, 5); len(evs) == 0 || evs[0].TemplateID != peaceAccordRow {
		t.Errorf("expected a peace-accord event (row %d) after phase 1, got %+v", peaceAccordRow, evs)
	}
	// Phase 1 is idempotent.
	lockedAt := got.LockedAt
	if err := ApplyDueSeasonEnd(ctx, store, repo, now); err != nil {
		t.Fatalf("apply (phase 1 repeat): %v", err)
	}
	if again, _ := LoadSeasonEnd(ctx, store); again.LockedAt != lockedAt {
		t.Error("phase 1 re-ran (lockedAt changed)")
	}

	// Advance into the maintenance window so phase 2 becomes due.
	later := t1.Add(time.Minute)
	if err := ApplyDueSeasonEnd(ctx, store, repo, later); err != nil {
		t.Fatalf("apply (phase 2): %v", err)
	}
	got, _ = LoadSeasonEnd(ctx, store)
	if got.AppliedAt == 0 {
		t.Fatal("phase 2 did not stamp appliedAt")
	}
	if SeasonNumber() != 15 {
		t.Errorf("season number = %d, want 15 after rollover", SeasonNumber())
	}
	var wantBf int64
	for i := 1; i < len(areaBattlefieldCount); i++ {
		wantBf += int64(areaBattlefieldCount[i])
	}
	if n, _ := repo.battlefields.CountDocuments(ctx, map[string]any{}); n != wantBf {
		t.Errorf("battlefields after reset = %d, want %d (reseeded)", n, wantBf)
	}
	if !SeasonLocked() {
		t.Error("maps should still be locked during the maintenance window (until t2)")
	}
	// Phase 2 idempotent.
	appliedAt := got.AppliedAt
	if err := ApplyDueSeasonEnd(ctx, store, repo, later.Add(time.Minute)); err != nil {
		t.Fatalf("apply (phase 2 repeat): %v", err)
	}
	if again, _ := LoadSeasonEnd(ctx, store); again.AppliedAt != appliedAt {
		t.Error("phase 2 re-ran (appliedAt changed)")
	}
}

func TestRefreshSeasonStateLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live season-refresh test")
	}
	ctx := context.Background()
	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)
	t.Cleanup(func() { _ = store.Collection(serverConfigCollection).Drop(ctx); ApplySeasonStart(0); ApplySeasonNumber(defaultSeasonNumber) })

	if err := SaveSeasonNumber(ctx, store, 21); err != nil {
		t.Fatalf("SaveSeasonNumber: %v", err)
	}
	future := time.Now().Add(time.Hour).Unix()
	if err := SaveSeasonStart(ctx, store, future); err != nil {
		t.Fatalf("SaveSeasonStart: %v", err)
	}
	ApplySeasonNumber(defaultSeasonNumber) // stale in-process value
	ApplySeasonStart(0)

	if err := RefreshSeasonState(ctx, store); err != nil {
		t.Fatalf("RefreshSeasonState: %v", err)
	}
	if SeasonNumber() != 21 {
		t.Errorf("season number = %d, want 21 after refresh", SeasonNumber())
	}
	if !SeasonLocked() {
		t.Error("expected locked after refresh (future season start)")
	}
}
