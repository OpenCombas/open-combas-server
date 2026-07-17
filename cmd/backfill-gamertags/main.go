// Command backfill-gamertags retroactively corrects combas gamertags (profiles + squad rosters) from the
// AUTHORITATIVE xuid->gamertag in the Xenia-WebServices `players` collection (set at each console's own
// login; same shared DB). This repairs the host-mis-sourced gamertags the 182 join builder stamps onto
// joiners, without waiting for each player to self-heal via a reg/login. Bounded by the players collection's
// 1-day TTL, so it fixes currently/recently-active players; the rest self-heal via RefreshGamertag.
//
//	go run ./cmd/backfill-gamertags          # DRY RUN — prints the plan, writes nothing
//	go run ./cmd/backfill-gamertags -apply   # execute
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/server"
	"context"
	"flag"
	"time"
)

func main() {
	apply := flag.Bool("apply", false, "execute the plan (default is a dry run that writes nothing)")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[BACKFILL] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[BACKFILL] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	squad := server.NewSquadRepository(store)
	report, err := squad.BackfillGamertags(ctx, store.Collection("players"), *apply)
	if err != nil {
		logging.Error.Fatalf("[BACKFILL] failed: %v", err)
	}

	mode := "DRY RUN (no writes)"
	if report.Applied {
		mode = "APPLIED"
	}
	logging.Info.Printf("[BACKFILL] %s — %d authoritative gamertags from webservices players", mode, report.PlayersScanned)
	printSection("PROFILE gamertag fixes", report.ProfileFixes)
	printSection("ROSTER gamertag fixes", report.RosterFixes)
	printSection("CLOBBER-SIGNATURE roster fixes (dup tag in roster -> join name)", report.ClobberRosterFixes)
	printSection("CLOBBER-SIGNATURE profile fixes", report.ClobberProfileFixes)
	total := len(report.ProfileFixes) + len(report.RosterFixes) + len(report.ClobberRosterFixes) + len(report.ClobberProfileFixes)
	if total == 0 {
		logging.Info.Printf("[BACKFILL] nothing to correct (all gamertags already match, or no login records)")
	} else if !report.Applied {
		logging.Info.Printf("[BACKFILL] dry run only — re-run with -apply to execute")
	}
}

func printSection(title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	logging.Info.Printf("[BACKFILL] %s (%d):", title, len(lines))
	for _, l := range lines {
		logging.Info.Printf("[BACKFILL]   - %s", l)
	}
}
