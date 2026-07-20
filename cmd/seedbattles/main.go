// Command seedbattles plays synthetic battles between the squads already in the database so the squad
// ranking screens (combas msgCode 202) have realistic, self-consistent history to render: win/loss
// counters, accumulated renown, and capture points that actually move BOTH ways.
//
// It drives the REAL server.BattleApplier with a nil world repository, so it runs the live
// CreditBattle + RefreshSquadGrade path -- including the loser's capture-point forfeit -- while leaving
// the war map completely untouched. That is the difference from cmd/simulate, which exists to exercise
// the world (occupation, locks, area cascades, news) and rewrites it as a side effect.
//
//	go run ./cmd/seedbattles                        # 200 battles across every squad, deterministic
//	go run ./cmd/seedbattles -battles 50 -seed 7    # fewer, different pairings
//	go run ./cmd/seedbattles -dry-run               # show what it would do, write nothing
//
// Battle magnitudes follow the wire limits: the battle report carries renown and occupation as single
// bytes (<= 255), and real captures show ~150 renown / ~100 occupation for a clean win, so the synthetic
// values stay in that range rather than inventing implausible numbers.
//
// Everything here is synthetic -- it only references team ids already present in the database and invents
// no player or squad names.
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/server"
	"context"
	"flag"
	"math/rand"
	"time"
)

func main() {
	battles := flag.Int("battles", 200, "number of synthetic battles to play")
	seed := flag.Int64("seed", 1, "RNG seed; the same seed replays the same battles")
	dryRun := flag.Bool("dry-run", false, "report the plan without writing anything")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[SEEDBATTLES] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[SEEDBATTLES] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	squadRepo := server.NewSquadRepository(store)

	teams, err := squadRepo.AllTeamIDs(ctx)
	if err != nil {
		logging.Error.Fatalf("[SEEDBATTLES] listing squads failed: %v", err)
	}
	if len(teams) < 2 {
		logging.Error.Fatalf("[SEEDBATTLES] need at least 2 squads, found %d", len(teams))
	}
	logging.Info.Printf("[SEEDBATTLES] %d squads available, playing %d battles (seed %d)", len(teams), *battles, *seed)

	if *dryRun {
		logging.Info.Printf("[SEEDBATTLES] dry run -- nothing written")
		return
	}

	// nil worldRepo: squad stats only, war map untouched. generateEvents=false for the same reason.
	// cpuBattleScale=1 so no PvE scaling is applied to these synthetic PvP results.
	applier := server.NewBattleApplier(nil, squadRepo, false, 1, "SEEDBATTLES")

	rng := rand.New(rand.NewSource(*seed))
	for i := 0; i < *battles; i++ {
		wi := rng.Intn(len(teams))
		li := rng.Intn(len(teams))
		for li == wi { // a squad cannot fight itself; CreditBattle skips that case anyway
			li = rng.Intn(len(teams))
		}

		applier.Apply(ctx, server.BattleResult{
			// Area/map are unused with a nil world repo, but set them to plausible values so the log lines
			// read like real reports.
			AreaID:       byte(rng.Intn(22) + 1),
			MapID:        byte(rng.Intn(4) + 1),
			WinnerNation: 'A' + byte(rng.Intn(3)),
			LoserNation:  'A' + byte(rng.Intn(3)),
			WinnerTeam:   teams[wi],
			LoserTeam:    teams[li],
			OccDelta:     int32(rng.Intn(101) + 20), // 20..120, report byte caps at 255
			WinnerMerit:  int32(rng.Intn(121) + 80), // 80..200, ~150 typical for a clean win
		})
	}

	logging.Info.Printf("[SEEDBATTLES] done: %d battles applied", *battles)
}
