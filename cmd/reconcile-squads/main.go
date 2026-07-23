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
//
// A second PRE-DEPLOY step behind -contributions migrates environments off the OLD flat-share withdraw model:
// the per-member renown ledger (RenownContribution) did not exist before, so on existing data every member
// reads 0 and a withdraw would debit nothing. This distributes each squad's UNATTRIBUTED renown (total minus
// what members already sum to) equally across the roster, so departures debit a real share. Idempotent; the
// past can't be attributed accurately, so an equal split is the honest approximation and new battles credit
// actual participants going forward. Run it ONCE after deploying the new binary.
//
//	go run ./cmd/reconcile-squads -contributions          # DRY RUN — show the per-squad distribution
//	go run ./cmd/reconcile-squads -contributions -apply   # seed the ledger
//
// Finally, -grant is a targeted compensation lever for a squad unfairly drained by the old model: it adds a
// renown amount to one team id and splits it evenly across the current roster (raising both the squad's
// ranking renown and each member's ledger, so it round-trips through a future withdraw). Requires -renown.
//
//	go run ./cmd/reconcile-squads -grant TM0001000000000042 -renown 500          # DRY RUN — show the split
//	go run ./cmd/reconcile-squads -grant TM0001000000000042 -renown 500 -apply   # execute
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
	contributions := flag.Bool("contributions", false, "backfill the per-member renown ledger from each squad's accrued renown (one-time migration from the old flat-share withdraw model)")
	grantTeam := flag.String("grant", "", "compensate ONE squad: add renown to this teamId and split it evenly among current members (for squads over-drained by the old model). Requires -renown.")
	grantRenown := flag.Int("renown", 0, "amount of renown to grant with -grant")
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

	// Load the DB war season so a -grant buckets renown under the current season (season.go). Best-effort:
	// keeps the default on a read error. The other modes only touch Renown.Total, so this doesn't affect them.
	if n, err := server.LoadSeasonNumber(ctx, store); err == nil {
		server.ApplySeasonNumber(n)
	}

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

	// Contribution backfill is its own pre-deploy step (like -grades): it seeds the per-member renown ledger
	// so departures debit a real share instead of 0 on data that predates the ledger. Run it ONCE, right
	// after deploying the new binary. Idempotent.
	if *contributions {
		cr, err := squad.BackfillMemberContributions(ctx, *apply)
		if err != nil {
			logging.Error.Fatalf("[RECONCILE] contribution backfill failed: %v", err)
		}
		mode := "DRY RUN (no writes)"
		if cr.Applied {
			mode = "APPLIED"
		}
		logging.Info.Printf("[RECONCILE] contribution backfill %s — %d squads scanned, %d with an unattributed renown gap", mode, cr.Scanned, len(cr.Changes))
		printSection("CONTRIBUTION BACKFILL (distribute unattributed renown equally across the roster)", cr.Changes)
		if !cr.Applied && len(cr.Changes) > 0 {
			logging.Info.Printf("[RECONCILE] dry run only — re-run with -contributions -apply to execute")
		}
		return
	}

	// Targeted compensation grant: add renown to one squad, split evenly across its current members.
	if *grantTeam != "" {
		if *grantRenown <= 0 {
			logging.Error.Fatalf("[RECONCILE] -grant requires -renown > 0 (got %d)", *grantRenown)
		}
		gr, err := squad.GrantSquadRenown(ctx, *grantTeam, int32(*grantRenown), *apply)
		if err != nil {
			logging.Error.Fatalf("[RECONCILE] grant failed: %v", err)
		}
		if !gr.Found {
			logging.Error.Fatalf("[RECONCILE] squad %q not found — nothing granted", *grantTeam)
		}
		mode := "DRY RUN (no writes)"
		if gr.Applied {
			mode = "APPLIED"
		}
		logging.Info.Printf("[RECONCILE] grant %s — %s: +%d renown across %d members (+%d each, remainder %d unattributed), season %s",
			mode, gr.TeamID, gr.Renown, gr.Members, gr.PerMember, gr.Remainder, server.SeasonKey(server.SeasonNumber()))
		if !gr.Applied {
			logging.Info.Printf("[RECONCILE] dry run only — re-run with -grant %s -renown %d -apply to execute", *grantTeam, *grantRenown)
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
