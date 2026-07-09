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
	"context"
	"flag"
	"time"
)

func main() {
	confirm := flag.Bool("confirm", false, "perform the destructive battlefield reset (required)")
	downscale := flag.Int("downscale", 1, "divide starting capture points (battlefield capacity) by this factor to scale the war to the playerbase size (>=1; e.g. 20 -> a 25000-point battlefield starts at 1250)")
	flag.Parse()

	if *downscale < 1 {
		logging.Error.Fatalf("[RESET] --downscale must be >= 1 (got %d)", *downscale)
	}

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[RESET] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	if !*confirm {
		logging.Warn.Printf("[RESET] battlefield reset is DESTRUCTIVE (discards war progression on database %q). Re-run with -confirm to proceed.", cfg.Mongo.Database)
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

	logging.Info.Printf("[RESET] running battlefield reset on database %q (downscale %d)...", cfg.Mongo.Database, *downscale)
	if err := reset.BattlefieldReset(ctx, store, int32(*downscale)); err != nil {
		logging.Error.Fatalf("[RESET] battlefield reset failed: %v", err)
	}
	logging.Info.Printf("[RESET] battlefield reset complete (downscale %d)", *downscale)
}
