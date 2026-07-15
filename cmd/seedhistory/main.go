// Command seedhistory writes a spread of synthetic squad-history events for one squad, so the in-game
// History screen (combas msgCode 199 / port 1259) has something to render without grinding joins/leaves.
// It drives the REAL repository (server.SquadRepository), so the rows are byte-identical to what the live
// server records and reads back -- correct BSON types, same code path.
//
//	go run ./cmd/seedhistory                                   # seed TM0001000000000001 (clears first)
//	go run ./cmd/seedhistory -team TM0001000000000002          # a different squad
//	go run ./cmd/seedhistory -interval 15 -no-clear            # 15 min apart, append to existing
//
// The events cover every rendered template type (2 joined, 3 left, 4/5 grade up/down, 6 invading,
// 7 defensive deployment). Names/locations are synthetic placeholders -- this is an open-source test tool.
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
	team := flag.String("team", "TM0001000000000001", "squad (team) id to seed history for")
	interval := flag.Int("interval", 60, "minutes between successive events")
	noClear := flag.Bool("no-clear", false, "append instead of clearing the squad's existing history first")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[SEED] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[SEED] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	squad := server.NewSquadRepository(store)
	if err := squad.EnsureSchema(ctx); err != nil {
		logging.Warn.Printf("[SEED] EnsureSchema: %v (continuing)", err)
	}

	if !*noClear {
		n, err := squad.ClearSquadHistory(ctx, *team)
		if err != nil {
			logging.Error.Fatalf("[SEED] clear failed: %v", err)
		}
		logging.Info.Printf("[SEED] cleared %d existing history rows for %s", n, *team)
	}

	// One synthetic squad story, oldest first. Each entry is stamped `interval` minutes after the previous,
	// so the last is ~now and the History screen (newest-first) leads with it.
	steps := []struct {
		desc string
		do   func(ctx context.Context, when time.Time) error
	}{
		{"Pilot-01 joined", func(c context.Context, w time.Time) error { return squad.RecordSquadJoined(c, *team, "Pilot-01", w) }},
		{"Pilot-02 joined", func(c context.Context, w time.Time) error { return squad.RecordSquadJoined(c, *team, "Pilot-02", w) }},
		{"Pilot-03 joined", func(c context.Context, w time.Time) error { return squad.RecordSquadJoined(c, *team, "Pilot-03", w) }},
		{"grade up -> Regular(4)", func(c context.Context, w time.Time) error { return squad.RecordSquadGrade(c, *team, 4, true, w) }},
		{"invading area 2 map 1", func(c context.Context, w time.Time) error { return squad.RecordSquadBattle(c, *team, 2, 1, true, w) }},
		{"defense area 5 map 2", func(c context.Context, w time.Time) error { return squad.RecordSquadBattle(c, *team, 5, 2, false, w) }},
		{"grade down -> Rookie++(3)", func(c context.Context, w time.Time) error { return squad.RecordSquadGrade(c, *team, 3, false, w) }},
		{"Pilot-02 left", func(c context.Context, w time.Time) error { return squad.RecordSquadLeft(c, *team, "Pilot-02", w) }},
	}

	step := time.Duration(*interval) * time.Minute
	base := time.Now().Add(-time.Duration(len(steps)-1) * step)
	for i, s := range steps {
		when := base.Add(time.Duration(i) * step)
		if err := s.do(ctx, when); err != nil {
			logging.Error.Fatalf("[SEED] %q failed: %v", s.desc, err)
		}
		logging.Info.Printf("[SEED] %s  @ %s", s.desc, when.Format("2006-01-02 15:04"))
	}
	logging.Info.Printf("[SEED] done: %d events for %s (newest first shows '%s')", len(steps), *team, steps[len(steps)-1].desc)
}
