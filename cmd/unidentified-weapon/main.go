// Command unidentified-weapon triggers the WORLD_NEWS stories for a nation's superweapon: it appears at a
// fixed battlefield, and later is either destroyed or withdrawn.
//
// The deployment site is NOT selectable. Those news rows carry no placeholder tokens -- the battlefield
// name is baked into the story text -- so each nation has exactly one site and choosing the nation chooses
// the location:
//
//	-nation A  Tarakia  -> Wakool
//	-nation B  Morskoj  -> East Salma Woods
//	-nation C  Sal Kar  -> South Cemo Oil Field
//
//	go run ./cmd/unidentified-weapon -nation B                     # DRY RUN: show the story, write nothing
//	go run ./cmd/unidentified-weapon -nation B -apply              # deploy the weapon (news: "Appears")
//	go run ./cmd/unidentified-weapon -nation B -phase destroy -apply
//	go run ./cmd/unidentified-weapon -nation B -phase withdraw -apply
//	go run ./cmd/unidentified-weapon -status                       # what each nation is currently doing
//
// Deploying also records Battlefield.WeaponNation. That is BOOKKEEPING ONLY -- it raises no wire flag and
// the client cannot currently see it. Use -news-only to skip it.
//
// マップロックフラグ was tried as the map-state lever and REJECTED by testing (2026-07-21): it does not
// switch the area-info preview to the _01 variant, it closes the battlefield to every nation including the
// attackers, which made the weapon unkillable. The real selector is still unfound.
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

// phaseScanLimit bounds how far back -status and the transition guard look in the news feed. A deployment
// buried deeper than this has scrolled out of the visible feed anyway.
const phaseScanLimit = 200

func main() {
	nation := flag.String("nation", "", "nation whose weapon this is: A (Tarakia), B (Morskoj) or C (Sal Kar)")
	phaseName := flag.String("phase", "appear", "appear | destroy | withdraw")
	apply := flag.Bool("apply", false, "write the event (default is a dry run that writes nothing)")
	status := flag.Bool("status", false, "report each nation's current weapon state and exit")
	force := flag.Bool("force", false, "skip the transition check (e.g. destroy a weapon with no recorded deployment)")
	newsOnly := flag.Bool("news-only", false, "publish the story without touching the battlefield's weapon state")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[WEAPON] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[WEAPON] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	repo := server.NewWorldRepository(store)

	if *status {
		reportStatus(ctx, repo)
		return
	}

	if strings.TrimSpace(*nation) == "" {
		logging.Error.Fatalf("[WEAPON] -nation is required (A, B or C); use -status to see current state")
	}
	code := strings.ToUpper(strings.TrimSpace(*nation))[0]
	site, ok := server.UnidentifiedWeaponSiteFor(code)
	if !ok {
		logging.Error.Fatalf("[WEAPON] unknown nation %q -- expected A (Tarakia), B (Morskoj) or C (Sal Kar)", *nation)
	}

	phase, ok := server.ParseWeaponPhase(*phaseName)
	if !ok {
		logging.Error.Fatalf("[WEAPON] unknown phase %q -- expected appear, destroy or withdraw", *phaseName)
	}

	row, _ := server.WeaponPhaseRow(code, phase)

	// Transition check. Deploying a weapon that is already deployed, or destroying one that was never
	// deployed, produces a story that reads as real but has no setup -- the news feed is player-facing
	// fiction, so an incoherent sequence is worse than a refused command.
	state, err := repo.CurrentWeaponPhase(ctx, code, phaseScanLimit)
	if err != nil {
		logging.Error.Fatalf("[WEAPON] state lookup failed: %v", err)
	}
	if problem := checkTransition(state, phase); problem != "" {
		if !*force {
			logging.Error.Fatalf("[WEAPON] %s -- re-run with -force to do it anyway", problem)
		}
		logging.Warn.Printf("[WEAPON] %s (proceeding: -force)", problem)
	}

	effect := "records deployment (bookkeeping only -- no client-visible effect)"
	if phase != server.WeaponAppears {
		effect = "clears deployment record"
	}
	if *newsOnly {
		effect = "news only -- deployment record untouched"
	}
	logging.Info.Printf("[WEAPON] %s (%c) weapon %s at %s (area %d map %d) -- WORLD_NEWS row %d; %s",
		site.NationName, site.Nation, phase, site.Battlefield, site.AreaID, site.MapID, row, effect)

	if !*apply {
		logging.Info.Printf("[WEAPON] dry run -- nothing written; re-run with -apply")
		return
	}

	// Battlefield state first, then the news. If the state write fails we stop before publishing a story
	// that claims a weapon exists -- news is player-visible and cannot be retracted, whereas a failed flag
	// write leaves the world consistent with the (unpublished) story.
	if !*newsOnly {
		var err error
		if phase == server.WeaponAppears {
			_, err = repo.SetWeaponDeployed(ctx, code)
		} else {
			_, err = repo.ClearWeaponDeployed(ctx, code)
		}
		if err != nil {
			logging.Error.Fatalf("[WEAPON] battlefield state update failed (no news published): %v", err)
		}
		logging.Info.Printf("[WEAPON] battlefield %d/%d deployment record %s",
			site.AreaID, site.MapID, map[bool]string{true: "set", false: "cleared"}[phase == server.WeaponAppears])
	}

	if _, err := repo.RecordUnidentifiedWeaponEvent(ctx, code, phase, time.Now()); err != nil {
		logging.Error.Fatalf("[WEAPON] failed to record event: %v", err)
	}
	logging.Info.Printf("[WEAPON] recorded.")
}

// checkTransition returns a human-readable problem, or "" when the transition is coherent.
func checkTransition(state server.WeaponPhaseState, next server.UnidentifiedWeaponPhase) string {
	switch next {
	case server.WeaponAppears:
		if state.Active {
			return "that nation already has a weapon deployed"
		}
	case server.WeaponDestroyed, server.WeaponWithdrawn:
		if !state.Found {
			return "no weapon deployment found for that nation in the recent feed"
		}
		if !state.Active {
			return "that nation's weapon is already " + state.Last.String()
		}
	}
	return ""
}

func reportStatus(ctx context.Context, repo *server.WorldRepository) {
	for _, code := range []byte{'A', 'B', 'C'} {
		site, _ := server.UnidentifiedWeaponSiteFor(code)
		state, err := repo.CurrentWeaponPhase(ctx, code, phaseScanLimit)
		if err != nil {
			logging.Error.Printf("[WEAPON] %s: lookup failed: %v", site.NationName, err)
			continue
		}
		// Report the battlefield flag as well as the news feed. They are written together but are separate
		// records, so a disagreement means a half-applied run -- worth seeing rather than inferring.
		deployed, derr := repo.WeaponDeployedNation(ctx, site.AreaID, site.MapID)
		flag := "clear"
		switch {
		case derr != nil:
			flag = "unreadable"
		case deployed == code:
			flag = "SET"
		case deployed != 0:
			flag = "set by " + string(deployed) + "?!"
		}

		news := "none"
		switch {
		case !state.Found:
			news = "no events"
		case state.Active:
			news = "DEPLOYED"
		default:
			news = state.Last.String()
		}

		warn := ""
		if (news == "DEPLOYED") != (flag == "SET") {
			warn = "   <-- news and battlefield flag DISAGREE"
		}
		logging.Info.Printf("[WEAPON] %-8s (%c) news: %-9s  flag: %-10s  site: %s (%d/%d)%s",
			site.NationName, code, news, flag, site.Battlefield, site.AreaID, site.MapID, warn)
	}
}
