// Command reconcile-squads restores the "one squad per player" invariant that the registration (181) and
// join (182) paths don't currently enforce: a player accumulates membership in every squad they ever
// created/joined because those paths repoint profile.teamId without removing them from the prior squad.
//
// For each player in >1 squad it KEEPS one (their profile.teamId squad if valid, else the one squad they
// lead, else the most recent) and removes them from the rest — pulling non-leaders, disbanding orphaned
// solo squads, and FLAGGING (never auto-touching) any squad where the player is a leader with followers.
// It then repairs every profile.teamId to match actual membership.
//
//	go run ./cmd/reconcile-squads            # DRY RUN — prints the plan, writes nothing
//	go run ./cmd/reconcile-squads -apply     # execute the plan
//
// It also carries an unrelated PRE-DEPLOY step behind -grades, which syncs each squad's stored grade with
// the value derived from its lifetime renown. Nothing renders the stored grade, so this is not a display
// fix: it exists so the first battle credit after a deploy does not mistake "never persisted" for "just
// promoted" and write a fictional grade-up into squad history. Run it BEFORE the new binary starts
// crediting battles. Idempotent.
//
//	go run ./cmd/reconcile-squads -grades           # DRY RUN — show which squads are out of sync
//	go run ./cmd/reconcile-squads -grades -apply    # write the corrected grades
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
	grades := flag.Bool("grades", false, "backfill stored squad grades from lifetime renown instead of reconciling membership")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[RECONCILE] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[RECONCILE] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	squad := server.NewSquadRepository(store)

	// Grade backfill runs on its own flag: it is a pre-deploy step with different timing requirements from
	// the membership reconcile (it must run BEFORE the new binary credits any battle), and bundling them
	// would force an operator wanting one to accept the other's writes.
	if *grades {
		gr, err := squad.BackfillSquadGrades(ctx, *apply)
		if err != nil {
			logging.Error.Fatalf("[RECONCILE] grade backfill failed: %v", err)
		}
		mode := "DRY RUN (no writes)"
		if gr.Applied {
			mode = "APPLIED"
		}
		logging.Info.Printf("[RECONCILE] grade backfill %s — %d squads scanned, %d out of sync", mode, gr.Scanned, len(gr.Changes))
		printSection("GRADE BACKFILL (stored grade -> derived; no history event)", gr.Changes)
		if !gr.Applied && len(gr.Changes) > 0 {
			logging.Info.Printf("[RECONCILE] dry run only — re-run with -grades -apply to execute")
		}
		return
	}

	report, err := squad.ReconcileSquads(ctx, *apply)
	if err != nil {
		logging.Error.Fatalf("[RECONCILE] failed: %v", err)
	}

	mode := "DRY RUN (no writes)"
	if report.Applied {
		mode = "APPLIED"
	}
	logging.Info.Printf("[RECONCILE] %s — %d squads scanned, %d players in >1 squad", mode, report.SquadsScanned, report.MultiSquadPlayers)
	printSection("PULLS (remove non-leader from extra squad)", report.Pulls)
	printSection("DISBANDS (delete orphaned solo squads)", report.Disbands)
	printSection("PROFILE FIXES (teamId -> actual membership)", report.ProfileFixes)
	printSection("FLAGS (MANUAL review — leader with followers, NOT touched)", report.Flags)

	if !report.Applied && (len(report.Pulls)+len(report.Disbands)+len(report.ProfileFixes) > 0) {
		logging.Info.Printf("[RECONCILE] dry run only — re-run with -apply to execute")
	}
}

func printSection(title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	logging.Info.Printf("[RECONCILE] %s (%d):", title, len(lines))
	for _, l := range lines {
		logging.Info.Printf("[RECONCILE]   - %s", l)
	}
}
