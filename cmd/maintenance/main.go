// Command maintenance sets, clears, or shows the server-wide maintenance window announced in the status
// reply (msgCode 187). One window exists server-wide (a singleton document), so this is the single lever for
// the login-time "out of service" announce.
//
//	go run ./cmd/maintenance                                  # show the stored window + what the client does
//	go run ./cmd/maintenance -in 24h -for 2h                  # window starting 24h from now, lasting 2h
//	go run ./cmd/maintenance -start 2026-08-01T02:00:00Z -end 2026-08-01T04:00:00Z
//	go run ./cmd/maintenance -clear                           # remove it (announce nothing)
//
// It also ends a WAR SEASON as a phased, automatic rollover (the server's season controller drives it):
//
//	go run ./cmd/maintenance -season-end -result 48h          # end now: lock maps + event, 48h result period, 6h maintenance
//	go run ./cmd/maintenance -season-end -lead 24h -result 48h -window 6h -downscale 20
//	go run ./cmd/maintenance -cancel-season-end               # cancel a scheduled season-end (before the window opens)
//
// IMPORTANT (see server/maintenanceWindow.go for the full RE): there is no "nothing scheduled" wire state --
// all-flags-zero is healthy AND shows the dated announce, so the title tells players about a window once per
// session by design. A window in the PAST must NOT be served: the client's availability predicate treats
// "maintenance ended" as the server being OFFLINE. This tool refuses to store an already-elapsed window and
// warns when a window would take the server offline.
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
	clear := flag.Bool("clear", false, "remove the scheduled window (the server announces nothing)")
	in := flag.Duration("in", 0, "schedule the window to START this long from now (e.g. 24h, 90m); pair with -for")
	forDur := flag.Duration("for", 2*time.Hour, "window DURATION when using -in (default 2h)")
	startStr := flag.String("start", "", "explicit window start (RFC3339, e.g. 2026-08-01T02:00:00Z); pair with -end")
	endStr := flag.String("end", "", "explicit window end (RFC3339); pair with -start")

	seasonEnd := flag.Bool("season-end", false, "END THE WAR SEASON: schedule the phased rollover -- lock maps + fire the end-of-war event at t0, then a maintenance window that increments the season and resets the battlefield. Uses -lead/-result/-window/-downscale.")
	lead := flag.Duration("lead", 0, "-season-end: delay from now until the season actually ends (t0). 0 = end immediately.")
	result := flag.Duration("result", 0, "-season-end: how long the ended season stays set with maps locked before maintenance begins (t0->t1). REQUIRED with -season-end.")
	window := flag.Duration("window", 6*time.Hour, "-season-end: maintenance window length during which the season increments and the battlefield resets (t1->t2; default 6h)")
	downscale := flag.Int("downscale", 1, "-season-end: battlefield-reset downscale for the NEW season (>=1)")
	cancelSeasonEnd := flag.Bool("cancel-season-end", false, "cancel a scheduled season-end (before the window opens): clears the schedule, unlocks maps, clears the maintenance window")
	force := flag.Bool("force", false, "-season-end: replace an already-pending season-end")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[MAINT] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[MAINT] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	repo := server.NewWorldRepository(store)
	now := time.Now().UTC()

	switch {
	case *cancelSeasonEnd:
		cancelSeasonEndFlow(ctx, store, repo)

	case *seasonEnd:
		scheduleSeasonEnd(ctx, store, repo, now, *lead, *result, *window, *downscale, *force)

	case *clear:
		if err := repo.ClearMaintenanceWindow(ctx); err != nil {
			logging.Error.Fatalf("[MAINT] clear failed: %v", err)
		}
		logging.Info.Printf("[MAINT] scheduled window cleared; the status server now serves its default future window (announces nothing actionable)")
		report(ctx, repo, now)

	case *in > 0 || *startStr != "" || *endStr != "":
		start, end := resolveWindow(*in, *forDur, *startStr, *endStr, now)
		if !end.After(now) {
			logging.Error.Fatalf("[MAINT] refusing to store an already-elapsed window (end %s <= now %s): the client would treat the server as OFFLINE. Schedule a future window or use -clear.",
				end.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		if err := repo.SetMaintenanceWindow(ctx, start, end); err != nil {
			logging.Error.Fatalf("[MAINT] set failed: %v", err)
		}
		st := server.ClassifyMaintenanceWindow(now, start, end)
		logging.Info.Printf("[MAINT] window set: %s .. %s (%s) -> client state: %s",
			start.Format(time.RFC3339), end.Format(time.RFC3339), end.Sub(start), st)
		if st == server.MaintInProgress || st == server.MaintEnded {
			logging.Warn.Printf("[MAINT] this window makes the server appear OFFLINE to clients right now")
		}

	default:
		report(ctx, repo, now)
	}
}

// resolveWindow turns the flag combination into a concrete (start, end). -start/-end win when given; else
// -in/-for. Fatal on an unparseable or incoherent combination.
func resolveWindow(in, forDur time.Duration, startStr, endStr string, now time.Time) (time.Time, time.Time) {
	if startStr != "" || endStr != "" {
		if startStr == "" || endStr == "" {
			logging.Error.Fatalf("[MAINT] -start and -end must be given together")
		}
		start, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			logging.Error.Fatalf("[MAINT] bad -start (want RFC3339 like 2026-08-01T02:00:00Z): %v", err)
		}
		end, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			logging.Error.Fatalf("[MAINT] bad -end (want RFC3339): %v", err)
		}
		return start.UTC(), end.UTC()
	}
	if forDur <= 0 {
		logging.Error.Fatalf("[MAINT] -for must be positive")
	}
	start := now.Add(in)
	return start, start.Add(forDur)
}

// report prints the stored window and the window actually served (which may differ -- an elapsed or absent
// stored window falls back to a default), plus the client state each produces.
func report(ctx context.Context, repo *server.WorldRepository, now time.Time) {
	stored, err := repo.GetMaintenanceWindow(ctx)
	if err != nil {
		logging.Error.Fatalf("[MAINT] read failed: %v", err)
	}
	if stored == nil {
		logging.Info.Printf("[MAINT] stored window: (none)")
	} else {
		st := server.ClassifyMaintenanceWindow(now, stored.Start, stored.End)
		logging.Info.Printf("[MAINT] stored window: %s .. %s -> %s",
			stored.Start.UTC().Format(time.RFC3339), stored.End.UTC().Format(time.RFC3339), st)
	}
	// What the status server would actually serve right now.
	start, end := repo.MaintenanceWindowFor(ctx, now)
	st := server.ClassifyMaintenanceWindow(now, start, end)
	logging.Info.Printf("[MAINT] served now:    %s .. %s -> %s",
		start.Format(time.RFC3339), end.Format(time.RFC3339), st)
}

// scheduleSeasonEnd stores the phased end-of-season plan and fires phase 1 (lock + event + window
// announce) immediately when -lead is 0. The server's season controller advances it from there: phase 1
// at t0, then the destructive rollover (season++ + battlefield reset) at t1, with maps auto-unlocking at
// t2. The outcome shown here is a PREVIEW from the current state; the applier recomputes it at t0.
func scheduleSeasonEnd(ctx context.Context, store *persistence.Store, repo *server.WorldRepository, now time.Time, lead, result, window time.Duration, downscale int, force bool) {
	switch {
	case result <= 0:
		logging.Error.Fatalf("[SEASON-END] -result must be positive (the period the ended season stays locked before maintenance)")
	case window <= 0:
		logging.Error.Fatalf("[SEASON-END] -window must be positive")
	case lead < 0:
		logging.Error.Fatalf("[SEASON-END] -lead must not be negative")
	case downscale < 1:
		logging.Error.Fatalf("[SEASON-END] -downscale must be >= 1")
	}

	existing, err := server.LoadSeasonEnd(ctx, store)
	if err != nil {
		logging.Error.Fatalf("[SEASON-END] read existing schedule failed: %v", err)
	}
	if existing.Pending() && !force {
		logging.Error.Fatalf("[SEASON-END] a season-end is already scheduled (t0=%s); pass -force to replace it, or -cancel-season-end first",
			time.Unix(existing.EndsAt, 0).UTC().Format(time.RFC3339))
	}

	cur, err := server.LoadSeasonNumber(ctx, store)
	if err != nil {
		logging.Error.Fatalf("[SEASON-END] read season number failed: %v", err)
	}
	t0 := now.Add(lead)
	t1 := t0.Add(result)
	t2 := t1.Add(window)
	sched := &server.SeasonEndSchedule{
		CreatedAt: now.Unix(), EndsAt: t0.Unix(), WindowStart: t1.Unix(), WindowEnd: t2.Unix(),
		FromSeason: cur, NextSeason: cur + 1, Downscale: int32(downscale),
	}

	if nations, err := repo.Nations(ctx); err == nil {
		kind, victor := server.DetermineOutcome(nations)
		logging.Info.Printf("[SEASON-END] preview (current standings): %s%s", outcomeLabel(kind), victorLabel(victor))
	}
	if err := server.SaveSeasonEnd(ctx, store, sched); err != nil {
		logging.Error.Fatalf("[SEASON-END] save schedule failed: %v", err)
	}
	// Fire phase 1 now if t0 has arrived (lead 0); a no-op while t0 is in the future (the server's
	// controller reaches it). Phase 2 (reset) is never due here -- result>0 keeps t1 in the future.
	if err := server.ApplyDueSeasonEnd(ctx, store, repo, now); err != nil {
		logging.Error.Fatalf("[SEASON-END] apply failed: %v", err)
	}

	logging.Info.Printf("[SEASON-END] scheduled: season %d -> %d (downscale %d)", cur, cur+1, downscale)
	logging.Info.Printf("[SEASON-END]   t0 season ends  : %s (in %s)", t0.UTC().Format(time.RFC3339), lead)
	logging.Info.Printf("[SEASON-END]   t1 maint. opens : %s (result period %s)", t1.UTC().Format(time.RFC3339), result)
	logging.Info.Printf("[SEASON-END]   t2 new season   : %s (maintenance %s) -> maps unlock, server available", t2.UTC().Format(time.RFC3339), window)
}

// cancelSeasonEndFlow removes a scheduled season-end and reverses its online-visible effects (unlock maps,
// clear the maintenance window). A rollover that has already incremented the season is not un-done -- only
// the record, lock, and window are cleared (it warns in that case).
func cancelSeasonEndFlow(ctx context.Context, store *persistence.Store, repo *server.WorldRepository) {
	existing, err := server.LoadSeasonEnd(ctx, store)
	if err != nil {
		logging.Error.Fatalf("[SEASON-END] read schedule failed: %v", err)
	}
	if existing == nil {
		logging.Info.Printf("[SEASON-END] nothing scheduled to cancel")
		return
	}
	if existing.AppliedAt != 0 {
		logging.Warn.Printf("[SEASON-END] the rollover already applied (season incremented at %s); cancel only clears the record, unlocks maps, and clears the window",
			time.Unix(existing.AppliedAt, 0).UTC().Format(time.RFC3339))
	}
	if err := server.ClearSeasonEnd(ctx, store); err != nil {
		logging.Error.Fatalf("[SEASON-END] clear schedule failed: %v", err)
	}
	if err := server.SaveSeasonStart(ctx, store, 0); err != nil {
		logging.Error.Fatalf("[SEASON-END] unlock maps failed: %v", err)
	}
	if err := repo.ClearMaintenanceWindow(ctx); err != nil {
		logging.Error.Fatalf("[SEASON-END] clear maintenance window failed: %v", err)
	}
	logging.Info.Printf("[SEASON-END] cancelled: schedule cleared, maps unlocked, maintenance window cleared")
}

func outcomeLabel(kind string) string {
	switch kind {
	case "victory":
		return "single-nation VICTORY"
	case "truce2":
		return "2-way truce"
	case "truce3":
		return "3-way truce"
	}
	return kind
}

func victorLabel(victor byte) string {
	switch victor {
	case 'A':
		return " (Tarakia)"
	case 'B':
		return " (Morskoj)"
	case 'C':
		return " (Sal Kar)"
	}
	return ""
}
