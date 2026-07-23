// Command maintenance sets, clears, or shows the server-wide maintenance window announced in the status
// reply (msgCode 187). One window exists server-wide (a singleton document), so this is the single lever for
// the login-time "out of service" announce.
//
//	go run ./cmd/maintenance                                  # show the stored window + what the client does
//	go run ./cmd/maintenance -in 24h -for 2h                  # window starting 24h from now, lasting 2h
//	go run ./cmd/maintenance -start 2026-08-01T02:00:00Z -end 2026-08-01T04:00:00Z
//	go run ./cmd/maintenance -clear                           # remove it (announce nothing)
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
