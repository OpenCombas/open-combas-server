// Command capture is an admin/testing tool that sets battlefield occupation directly in the world DB, so a
// nation can be made to control an area (or a single battlefield) without grinding missions by hand.
//
// It mutates occupation STATE ONLY (for exercising the war-map / area-info display) and clears any capture
// lock -- it does NOT run game logic or emit events. To simulate real battle behaviour (captures, locks,
// area cascades, HQ dissolution/revival, and the news events they raise), use `cmd/simulate`, which drives
// the live server's own BattleApplier.
//
// Capture is winner-takes-all: the chosen nation gets `-level` percent of each battlefield's capacity and the
// other two nations are zeroed (the clean single-occupant state the display renders correctly). For a mixed
// area, run it per battlefield with -map.
//
//	go run ./cmd/capture -area 7 -nation C                 # C fully captures every battlefield in area 7
//	go run ./cmd/capture -area 7 -map 3 -nation A          # A captures just battlefield 3
//	go run ./cmd/capture -area 7 -nation B -level 60       # B holds 60% of each battlefield (still sole occupant)
//	go run ./cmd/capture -area 7 -list                     # print area 7's current occupation, change nothing
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const battlefieldsCollection = "battlefields"

// battlefield mirrors the fields of the battlefields collection this tool reads/writes.
type battlefield struct {
	AreaID   int32 `bson:"areaId"`
	MapID    int32 `bson:"mapId"`
	Capacity int32 `bson:"capacity"`
	OccA     int32 `bson:"occA"`
	OccB     int32 `bson:"occB"`
	OccC     int32 `bson:"occC"`
}

// occIndex maps a nation letter to the occ slot index (A=0 Tarakia, B=1 Morskoj, C=2 Sal Kar).
func occIndex(nation string) (int, bool) {
	switch strings.ToUpper(nation) {
	case "A":
		return 0, true
	case "B":
		return 1, true
	case "C":
		return 2, true
	}
	return 0, false
}

func main() {
	area := flag.Int("area", 0, "area id to operate on (1-22, required)")
	mapID := flag.Int("map", 0, "battlefield id within the area; 0 = all battlefields in the area")
	nation := flag.String("nation", "", "capturing nation: A (Tarakia) | B (Morskoj) | C (Sal Kar)")
	level := flag.Int("level", 100, "occupation percent (1-100) to give the capturing nation; the other two are zeroed")
	list := flag.Bool("list", false, "only print the target's current occupation, make no changes")
	flag.Parse()

	if *area < 1 || *area > 22 {
		logging.Error.Fatalf("[CAPTURE] -area must be 1-22 (got %d)", *area)
	}
	if *level < 1 || *level > 100 {
		logging.Error.Fatalf("[CAPTURE] -level must be 1-100 (got %d)", *level)
	}

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[CAPTURE] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[CAPTURE] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	coll := store.Collection(battlefieldsCollection)

	filter := bson.M{"areaId": int32(*area)}
	if *mapID > 0 {
		filter["mapId"] = int32(*mapID)
	}

	bfs, err := load(ctx, coll, filter)
	if err != nil {
		logging.Error.Fatalf("[CAPTURE] query failed: %v", err)
	}
	if len(bfs) == 0 {
		logging.Error.Fatalf("[CAPTURE] no battlefields found for area %d%s", *area, mapSuffix(*mapID))
	}

	if *list {
		printState("current:", bfs)
		return
	}

	idx, ok := occIndex(*nation)
	if !ok {
		logging.Error.Fatalf("[CAPTURE] -nation is required and must be A, B, or C (got %q)", *nation)
	}

	printState("BEFORE:", bfs)

	for _, b := range bfs {
		var occ [3]int32
		occ[idx] = b.Capacity * int32(*level) / 100
		if _, err := coll.UpdateOne(ctx,
			bson.M{"areaId": b.AreaID, "mapId": b.MapID},
			// Clear any capture lock so an admin override always leaves the battlefield contestable
			// again (this tool is for setting up test scenarios, not for locking anything out).
			bson.M{
				"$set":   bson.M{"occA": occ[0], "occB": occ[1], "occC": occ[2], "locked": false},
				"$unset": bson.M{"defeatedNation": "", "unlockAtBattle": ""},
			},
		); err != nil {
			logging.Error.Fatalf("[CAPTURE] update area %d/%d failed: %v", b.AreaID, b.MapID, err)
		}
	}

	after, _ := load(ctx, coll, filter)
	printState(fmt.Sprintf("AFTER (nation %s @ %d%%):", strings.ToUpper(*nation), *level), after)
	logging.Info.Printf("[CAPTURE] captured %d battlefield(s) in area %d for nation %s", len(bfs), *area, strings.ToUpper(*nation))
}

// load reads the battlefields matching filter, sorted by mapId for stable output.
func load(ctx context.Context, coll *mongo.Collection, filter bson.M) ([]battlefield, error) {
	cur, err := coll.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	var bfs []battlefield
	if err := cur.All(ctx, &bfs); err != nil {
		return nil, err
	}
	sort.Slice(bfs, func(i, j int) bool { return bfs[i].MapID < bfs[j].MapID })
	return bfs, nil
}

// printState prints a compact per-battlefield occupation table with the current leader.
func printState(header string, bfs []battlefield) {
	fmt.Println(header)
	for _, b := range bfs {
		fmt.Printf("  area %d map %d  cap %-6d  A=%-6d B=%-6d C=%-6d  -> leader %c\n",
			b.AreaID, b.MapID, b.Capacity, b.OccA, b.OccB, b.OccC, leaderChar(b))
	}
}

// leaderChar returns the controlling nation letter for a battlefield, or '-' if unoccupied.
func leaderChar(b battlefield) byte {
	best, ch := b.OccA, byte('A')
	if b.OccB > best {
		best, ch = b.OccB, 'B'
	}
	if b.OccC > best {
		best, ch = b.OccC, 'C'
	}
	if best == 0 {
		return '-'
	}
	return ch
}

func mapSuffix(mapID int) string {
	if mapID > 0 {
		return fmt.Sprintf(" map %d", mapID)
	}
	return ""
}
