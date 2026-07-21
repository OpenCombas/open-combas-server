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
// SCOPE: this emits the NEWS only. The stories say the nation bans its own mercenaries from that
// battlefield while the weapon stands; that gameplay effect is not implemented, so the ban is currently
// narrative. See server/unidentifiedWeapon.go for the battlefield ids a future lock would need.
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

	logging.Info.Printf("[WEAPON] %s (%c) weapon %s at %s -- WORLD_NEWS row %d",
		site.NationName, site.Nation, phase, site.Battlefield, row)

	if !*apply {
		logging.Info.Printf("[WEAPON] dry run -- nothing written; re-run with -apply")
		return
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
		switch {
		case !state.Found:
			logging.Info.Printf("[WEAPON] %-8s (%c) no weapon events on record       site: %s",
				site.NationName, code, site.Battlefield)
		case state.Active:
			logging.Info.Printf("[WEAPON] %-8s (%c) DEPLOYED                         site: %s",
				site.NationName, code, site.Battlefield)
		default:
			logging.Info.Printf("[WEAPON] %-8s (%c) last event: %-9s          site: %s",
				site.NationName, code, state.Last, site.Battlefield)
		}
	}
}
