// Command simulate-weapon builds the world state an unidentified weapon is HYPOTHESISED to require, so
// the hypothesis can actually be tested in-game.
//
// THE HYPOTHESIS (operator, 2026-07-21 -- NOT confirmed): retail deployed a nation's superweapon once that
// nation was reduced to <= 10 occupation points ("orange dots") across the whole map and had just retaken
// its weapon area from an occupier.
//
// The threshold is structurally suggestive: every area distributes exactly 5 dots, so <= 10 points is
// precisely "down to two areas". For Sal Kar that is its HQ (area 3, Qara) plus its weapon area (18,
// containing South Cemo Oil Field) -- a last stand.
//
//	go run ./cmd/simulate-weapon -nation C                       # DRY RUN, prints the plan
//	go run ./cmd/simulate-weapon -nation C -apply                # build the state
//	go run ./cmd/simulate-weapon -nation C -apply -recapture     # + publish the "just retaken" news
//
// This is SIMULATION SETUP, not gameplay logic. Nothing here decides when a weapon appears; it only
// arranges the world so we can observe whether the client does. It rewrites EVERY battlefield, so run it
// against a test database, not production.
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/server"
	"context"
	"flag"
	"strings"
	"time"
)

func main() {
	nationFlag := flag.String("nation", "", "nation making the last stand: A (Tarakia), B (Morskoj) or C (Sal Kar)")
	conqFlag := flag.String("conqueror", "", "nation holding everything else (default: B, or A when -nation B)")
	apply := flag.Bool("apply", false, "write the state (default is a dry run that writes nothing)")
	recapture := flag.Bool("recapture", false, "also publish the 'just retaken the weapon area' news event")
	flag.Parse()

	if strings.TrimSpace(*nationFlag) == "" {
		logging.Error.Fatalf("[SIMWEAPON] -nation is required (A, B or C)")
	}
	nation := strings.ToUpper(strings.TrimSpace(*nationFlag))[0]
	site, ok := server.UnidentifiedWeaponSiteFor(nation)
	if !ok {
		logging.Error.Fatalf("[SIMWEAPON] unknown nation %q", *nationFlag)
	}

	// Default conqueror: B normally, A when the last-stand nation IS B.
	conqueror := byte('B')
	if nation == 'B' {
		conqueror = 'A'
	}
	if s := strings.TrimSpace(*conqFlag); s != "" {
		conqueror = strings.ToUpper(s)[0]
	}

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[SIMWEAPON] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[SIMWEAPON] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	repo := server.NewWorldRepository(store)

	plan, err := repo.WeaponLastStand(ctx, nation, conqueror, *apply)
	if err != nil {
		logging.Error.Fatalf("[SIMWEAPON] failed: %v", err)
	}

	mode := "DRY RUN (no writes)"
	if *apply {
		mode = "APPLIED"
	}
	logging.Info.Printf("[SIMWEAPON] %s -- %s (%c) last stand", mode, site.NationName, site.Nation)
	logging.Info.Printf("[SIMWEAPON]   holds  : area %d (HQ) + area %d (weapon site: %s)",
		plan.HQArea, plan.WeaponArea, site.Battlefield)
	logging.Info.Printf("[SIMWEAPON]   cedes  : %d battlefields to %c", len(plan.Ceded), plan.Conqueror)
	logging.Info.Printf("[SIMWEAPON]   keeps  : %d battlefields", len(plan.Held))
	logging.Info.Printf("[SIMWEAPON]   OCCUPATION POINTS after: %s = %d  (hypothesis wants <= 10)",
		site.NationName, plan.DotsAfter)
	if len(plan.Unlocked) > 0 {
		logging.Info.Printf("[SIMWEAPON]   capture locks cleared on the weapon area: %v", plan.Unlocked)
	}

	if !*apply {
		logging.Info.Printf("[SIMWEAPON] dry run -- re-run with -apply to build it")
		return
	}

	if *recapture {
		// "Just retaken from an occupier": the region-capture story names the nation that ABANDONED the
		// area, which in this scenario is the conqueror being pushed back out of the weapon area.
		if err := repo.RecordRegionRecapture(ctx, plan.WeaponArea, plan.Conqueror, time.Now()); err != nil {
			logging.Error.Fatalf("[SIMWEAPON] recapture news failed: %v", err)
		}
		logging.Info.Printf("[SIMWEAPON]   published the '%c abandons area %d' recapture story", plan.Conqueror, plan.WeaponArea)
	}

	logging.Info.Printf("[SIMWEAPON] done. Re-login on the console so the world/area data is refetched.")
}
