// Command reset initializes / resets the combas world state out-of-band. It is deliberately separate from
// the UDP server, which no longer seeds the battlefield war state on boot -- resetting is a destructive
// "new season" operation that must be run explicitly.
//
// It connects to the same MongoDB the server uses (resolved from config.toml + the MONGO_URI /
// MONGO_DATABASE env vars) and runs the requested reset. Battlefield reset discards accumulated war
// progression, so it requires an explicit -confirm.
//
//	go run ./cmd/reset -confirm     # from the server module directory
//	/app/reset -confirm             # inside the container image (WORKDIR /app has config.toml)
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/reset"
	"ChromehoundsStatusServer/server"
	"context"
	"flag"
	"time"
)

func main() {
	confirm := flag.Bool("confirm", false, "perform the destructive reset (required)")
	downscale := flag.Int("downscale", 1, "divide starting capture points (battlefield capacity) by this factor to scale the war to the playerbase size (>=1; e.g. 20 -> a 25000-point battlefield starts at 1250)")
	only := flag.String("only", "", "reset ONLY one subsystem instead of the whole world: battlefields | events | captures (default: all). Lets a feature be re-tested mid-season without wiping unrelated state")
	season := flag.Int("season", 0, "set the server-wide war season number (>=1); reflected in the squad-stats aggregation buckets and the Status/World Season IDs at the next server start. 0 = leave unchanged. Not destructive, so it works without -confirm.")
	lockout := flag.Duration("lockout-window", 0, "lock ALL maps for deployment for this long from now -- the between-seasons window during which squad leaders can change allegiance (e.g. 48h). Maps auto-unlock when it elapses. Non-destructive (works without -confirm); a -confirm world/battlefields reset with no -lockout-window opens the new season live immediately.")
	flag.Parse()

	if *downscale < 1 {
		logging.Error.Fatalf("[RESET] --downscale must be >= 1 (got %d)", *downscale)
	}

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[RESET] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	scope := *only
	if scope == "" {
		scope = "all"
	}
	if !*confirm && *season <= 0 && *lockout <= 0 {
		logging.Warn.Printf("[RESET] nothing to do. The reset (%s) is DESTRUCTIVE on database %q -- re-run with -confirm; or set just the war season with -season N, or open a lockout window with -lockout-window D.", scope, cfg.Mongo.Database)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[RESET] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	// Setting the season is non-destructive, so it runs whether or not the world reset is confirmed. It takes
	// effect on the NEXT server start (the servers load it at boot), which is when a new season begins.
	if *season > 0 {
		if err := server.SaveSeasonNumber(ctx, store, *season); err != nil {
			logging.Error.Fatalf("[RESET] set season failed: %v", err)
		}
		logging.Info.Printf("[RESET] war season set to %d (applies at the next server start)", *season)
	}

	// The between-seasons lockout is non-destructive (like -season): if given, lock ALL maps for deployment
	// from now for the window. Applies at the next server start and auto-unlocks when it elapses.
	if *lockout > 0 {
		startAt := time.Now().Add(*lockout)
		if err := server.SaveSeasonStart(ctx, store, startAt.Unix()); err != nil {
			logging.Error.Fatalf("[RESET] set lockout window failed: %v", err)
		}
		logging.Info.Printf("[RESET] all maps LOCKED for deployment for %s (season starts %s) -- allegiance-change window; auto-unlocks after (applies at next server start)", *lockout, startAt.Format("2006-01-02 15:04 MST"))
	}

	if !*confirm {
		return
	}

	logging.Info.Printf("[RESET] running reset (%s) on database %q (downscale %d)...", scope, cfg.Mongo.Database, *downscale)
	if err := reset.Reset(ctx, store, int32(*downscale), *only); err != nil {
		logging.Error.Fatalf("[RESET] reset failed: %v", err)
	}
	logging.Info.Printf("[RESET] reset (%s) complete", scope)

	// A world/battlefields reset opens a new season. Unless the operator explicitly opened a lockout above,
	// clear any stale one so the fresh season is live immediately.
	if *lockout == 0 && (scope == "all" || scope == "battlefields" || scope == "world") {
		if err := server.SaveSeasonStart(ctx, store, time.Now().Unix()); err != nil {
			logging.Error.Fatalf("[RESET] clear lockout window failed: %v", err)
		}
	}
}
