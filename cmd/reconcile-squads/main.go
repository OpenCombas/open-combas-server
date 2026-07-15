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
