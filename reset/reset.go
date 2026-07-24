// Package reset holds the out-of-band reset / initialization logic for the combas world state.
//
// The UDP server intentionally does NOT initialize or reset the war state on boot. Seeding the
// battlefield/area occupation is a deliberate, destructive operation -- a "new season" / war restart that
// discards accumulated progression -- so it must be run explicitly (a separate reset command/executable)
// rather than implicitly every time the server starts. This package centralizes that logic; a thin
// executable is expected to wire flags/config to these functions.
//
// Each reset concern gets its own entry point. Battlefield reset is the first.
package reset

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	battlefieldsCollection = "battlefields"
	eventsCollection       = "events"
	capturesCollection     = "captureContributions"
	nationsCollection      = "nations"
	areaStatsCollection    = "areaBattleStats"

	// defaultCapturePoints is the occupation-pool capacity used for a battlefield whose real capture
	// points aren't known yet (known_occ_points lists a 0 for those).
	defaultCapturePoints = 25000
)

// battlefield is one stored occupation document. Its field / bson layout MUST match server.Battlefield
// (the war-state schema the UDP servers read); kept local so this package stays decoupled from server.
type battlefield struct {
	AreaID         int32 `bson:"areaId"`
	MapID          int32 `bson:"mapId"`
	Capacity       int32 `bson:"capacity"`       // occupation-pool max == a battlefield's capture points
	StrategicValue int32 `bson:"strategicValue"` // orange dots (income weight); not in known_occ_points -> 0 for now
	OccA           int32 `bson:"occA"`           // Tarakia occupation
	OccB           int32 `bson:"occB"`           // Morskoj
	OccC           int32 `bson:"occC"`           // Sal Kar
}

// areaBattlefieldCount[N] = number of battlefields in area N (1..22), from mapdata/map/mNN_*. Mirrors the
// server's static model and was validated against known_occ_points.png (per-area map counts match).
var areaBattlefieldCount = [23]byte{
	0,                            // index 0 unused (areas are 1-based)
	4, 4, 3, 3, 3, 4, 3, 4, 4, 4, // 1-10
	4, 4, 3, 3, 4, 3, 4, 4, 3, 4, // 11-20
	4, 4, // 21-22
}

// areaDefaultNation[N] = the nation that owns 100% of every battlefield in area N at reset (0=A/Tarakia,
// 1=B/Morskoj, 2=C/Sal Kar). Read from init_state.png flag colours (A=blue/white, B=red/black/white,
// C=green/yellow) after chromehounds_battlefields_areas.csv pinned each area id to its strategic-map node.
// The three nations start on contiguous blocks: B holds the north, A the west/southwest, C the southeast.
var areaDefaultNation = [23]byte{
	0, // index 0 unused
	0, // 1  Xeres        -> A
	1, // 2  Ostrov       -> B
	2, // 3  Qara         -> C
	1, // 4  Tartat       -> B
	1, // 5  Drozdovka    -> B
	1, // 6  Dunaj        -> B
	1, // 7  Olenya Guba  -> B
	1, // 8  Mejgorye     -> B
	0, // 9  Albury       -> A
	0, // 10 Tajin        -> A
	0, // 11 Baleares     -> A
	0, // 12 Braidwood    -> A
	0, // 13 Melton       -> A
	0, // 14 Saint Yves   -> A
	0, // 15 Mortlake     -> A
	1, // 16 Elani        -> B
	1, // 17 Xivera       -> B
	2, // 18 Gazi         -> C
	2, // 19 Him Hime     -> C
	0, // 20 Berwick      -> A
	2, // 21 Trebizond    -> C
	2, // 22 Tamala       -> C
}

// canonicalBattlefields is the authoritative launch-time ("OLD") data for every one of the 80 battlefields,
// transcribed from the Chromehounds map list keyed by Map File Number (AA_BBB = area AA, battlefield BBB):
//   - Capacity == "War Point Value (OLD)" -- the occupation-pool capacity == a battlefield's capture level.
//   - Dots     == "# Of Points (OLD)"     -- the strategic-value ("orange dots"); every area's dots sum to 5.
// The battlefield id ordering is authoritative from chromehounds_battlefields_areas.csv (name<->id); the map
// list was keyed by name and its Map File Numbers were corrected to match, so (area,map) here lines up with
// the ids the rest of the server uses. This supersedes the earlier partial known_occ_points.png transcription
// (which had ~12 wrong capacities). Every battlefield is present, so the capacityFor/dotsFor fallbacks below
// are belt-and-suspenders only.
var canonicalBattlefields = map[[2]int32]struct{ Capacity, Dots int32 }{
	{1, 1}:  {50000, 2}, // Central Xeres
	{1, 2}:  {40000, 1}, // West Xeres
	{1, 3}:  {40000, 1}, // East Xeres
	{1, 4}:  {40000, 1}, // South Xeres
	{2, 1}:  {40000, 1}, // North Ostrov
	{2, 2}:  {50000, 2}, // Central Ostrov
	{2, 3}:  {40000, 1}, // East Ostrov
	{2, 4}:  {40000, 1}, // West Ostrov
	{3, 1}:  {50000, 2}, // Central Qara
	{3, 2}:  {50000, 2}, // North Qara
	{3, 3}:  {40000, 1}, // South Qara
	{4, 1}:  {38000, 2}, // Fort Ozero
	{4, 2}:  {38000, 2}, // Fort Perimeter
	{4, 3}:  {30000, 1}, // Fort Snowfield
	{5, 1}:  {30000, 1}, // Zavoywko Dam 1
	{5, 2}:  {38000, 2}, // Olensk Highlands
	{5, 3}:  {38000, 2}, // Zavoywko Dam 2
	{6, 1}:  {35000, 2}, // Ileckaya Base Ruins
	{6, 2}:  {25000, 1}, // Base Ruins Perimeter
	{6, 3}:  {25000, 1}, // Kudelyyka Valley
	{6, 4}:  {25000, 1}, // Xikhranwi Highlands
	{7, 1}:  {10000, 1}, // Sulimov Castle Wall
	{7, 2}:  {20000, 2}, // Northern Walls
	{7, 3}:  {20000, 2}, // Southern Walls
	{8, 1}:  {33000, 2}, // East Mount Catana
	{8, 2}:  {25000, 1}, // West Mount Catana
	{8, 3}:  {25000, 1}, // North Mount Catana
	{8, 4}:  {25000, 1}, // South Mount Catana
	{9, 1}:  {33000, 2}, // Stanly Mountains
	{9, 2}:  {25000, 1}, // Loxton Gorge
	{9, 3}:  {25000, 1}, // Upstream Of Gorge
	{9, 4}:  {25000, 1}, // Edina Mine Ruins
	{10, 1}: {10000, 1}, // North Village Ruin
	{10, 2}: {10000, 1}, // South Village Ruin
	{10, 3}: {10000, 1}, // Southworth Highway
	{10, 4}: {20000, 2}, // Village Ruin
	{11, 1}: {38000, 2}, // East Cale Plains
	{11, 2}: {32000, 1}, // West Cale Plains
	{11, 3}: {32000, 1}, // Maldon
	{11, 4}: {32000, 1}, // Wakool
	{12, 1}: {35000, 2}, // Cobar
	{12, 2}: {25000, 1}, // North Bath Plains
	{12, 3}: {25000, 1}, // South Bath Plains
	{12, 4}: {25000, 1}, // Ebus Lake
	{13, 1}: {38000, 2}, // Osca Industrial Zone
	{13, 2}: {38000, 2}, // East Industrial Zone
	{13, 3}: {30000, 1}, // West Industrial Zone
	{14, 1}: {38000, 2}, // North Stanthorpe Bay
	{14, 2}: {30000, 1}, // South Stanthorpe Bay
	{14, 3}: {38000, 2}, // Bay Warehouse District
	{15, 1}: {33000, 2}, // Cecil Plains
	{15, 2}: {25000, 1}, // East Cecil Plains
	{15, 3}: {25000, 1}, // Kilmore Highlands
	{15, 4}: {25000, 1}, // North Cecil Plains
	{16, 1}: {38000, 2}, // Lake Orlovka
	{16, 2}: {38000, 2}, // East Lake Orlovka
	{16, 3}: {30000, 1}, // Lake Suhodol
	{17, 1}: {38000, 2}, // Savonovo
	{17, 2}: {32000, 1}, // Eger
	{17, 3}: {32000, 1}, // West Salma Woods
	{17, 4}: {32000, 1}, // East Salma Woods
	{18, 1}: {38000, 2}, // North Cemo Oil Fields
	{18, 2}: {32000, 1}, // Dinar Plains
	{18, 3}: {32000, 1}, // North Dinar Plains
	{18, 4}: {32000, 1}, // South Cemo Oil Field
	{19, 1}: {30000, 2}, // West Bayazit River
	{19, 2}: {20000, 1}, // East Bayazit River
	{19, 3}: {30000, 2}, // Biraal Water Plant
	{20, 1}: {10000, 1}, // Ruins
	{20, 2}: {20000, 2}, // Old Berri Coal Mine
	{20, 3}: {10000, 1}, // North Cressy Desert
	{20, 4}: {10000, 1}, // South Cressy Desert
	{21, 1}: {38000, 2}, // Kara Bakir
	{21, 2}: {28000, 1}, // Bas Bar
	{21, 3}: {28000, 1}, // North Durama Desert
	{21, 4}: {28000, 1}, // South Durama Desert
	{22, 1}: {38000, 2}, // Pamak Mine
	{22, 2}: {28000, 1}, // Aydin Desert
	{22, 3}: {28000, 1}, // Bais Halayi Field
	{22, 4}: {28000, 1}, // South Mine
}

// areaDotTotal documents the invariant that every area's strategic-value dots sum to 5 (the canonical data
// above satisfies it; the tests assert it).
const areaDotTotal = 5

// capacityFor returns a battlefield's occupation-pool capacity (its launch-time War Point Value); missing
// battlefields fall back to the default (never expected -- the canonical table is complete).
func capacityFor(area, mapID int32) int32 {
	if c, ok := canonicalBattlefields[[2]int32{area, mapID}]; ok {
		return c.Capacity
	}
	return defaultCapturePoints
}

// dotsFor returns a battlefield's strategic value ("orange dots" == its launch-time "# Of Points"); missing
// battlefields fall back to 1 (never expected -- the canonical table is complete).
func dotsFor(area, mapID int32) int32 {
	if c, ok := canonicalBattlefields[[2]int32{area, mapID}]; ok {
		return c.Dots
	}
	return 1
}

// seedBattlefields builds the full reset layout: every battlefield in every area, 100% occupied by the
// area's default nation, with capacity == its capture points (or the 25000 default).
//
// downscale divides every battlefield's starting capture points (capacity, and the default-nation
// occupation that fills it) so the overall war scale can be matched to the playerbase size -- e.g.
// downscale 20 turns a 25000-point battlefield into 1250, so a smaller playerbase can still flip areas.
// A downscale < 1 is treated as 1 (no change); capacity is floored at 1 so a battlefield never becomes
// an uncapturable / zero-capacity pool.
func seedBattlefields(downscale int32) []battlefield {
	if downscale < 1 {
		downscale = 1
	}
	var out []battlefield
	for area := int32(1); int(area) < len(areaBattlefieldCount); area++ {
		count := int32(areaBattlefieldCount[area])
		nation := areaDefaultNation[area]
		for mapID := int32(1); mapID <= count; mapID++ {
			cap := capacityFor(area, mapID) / downscale
			if cap < 1 {
				cap = 1
			}
			bf := battlefield{AreaID: area, MapID: mapID, Capacity: cap, StrategicValue: dotsFor(area, mapID)}
			switch nation {
			case 0:
				bf.OccA = cap // 100% nation A
			case 1:
				bf.OccB = cap // 100% nation B
			case 2:
				bf.OccC = cap // 100% nation C
			}
			out = append(out, bf)
		}
	}
	return out
}

// SeedBattlefield is a public view of one reset battlefield, for out-of-band tooling (e.g. the season
// predictor) that needs the canonical starting layout without depending on Mongo. Owner is the nation char
// ('A'/'B'/'C') that holds it 100% at reset.
type SeedBattlefield struct {
	AreaID, MapID, Capacity, StrategicValue int32
	Owner                                   byte
}

// SeededBattlefields returns the canonical starting battlefield layout at the given downscale, exactly as
// resetBattlefields would write it (same seedBattlefields path -> single source of truth for the canonical
// capacities/dots and default ownership).
func SeededBattlefields(downscale int32) []SeedBattlefield {
	raw := seedBattlefields(downscale)
	out := make([]SeedBattlefield, len(raw))
	for i, b := range raw {
		owner := byte('A')
		switch {
		case b.OccB == b.Capacity:
			owner = 'B'
		case b.OccC == b.Capacity:
			owner = 'C'
		}
		out[i] = SeedBattlefield{AreaID: b.AreaID, MapID: b.MapID, Capacity: b.Capacity, StrategicValue: b.StrategicValue, Owner: owner}
	}
	return out
}

// Reset runs the requested subsystem reset. only == "" or "all" resets the WHOLE world (a new season:
// battlefields + events + captures). A specific subsystem ("battlefields"/"world", "events"/"news", or
// "captures") resets ONLY that one, so a feature can be re-tested mid-season WITHOUT wiping unrelated
// accumulated state -- e.g. re-seed the news feed while keeping the war map. downscale applies only to
// the battlefield reset. All parts are DESTRUCTIVE for the state they touch.
func Reset(ctx context.Context, store *persistence.Store, downscale int32, only string) error {
	switch only {
	case "", "all":
		if err := resetBattlefields(ctx, store, downscale); err != nil {
			return err
		}
		if err := resetEvents(ctx, store); err != nil {
			return err
		}
		return resetCaptures(ctx, store)
	case "battlefields", "world":
		return resetBattlefields(ctx, store, downscale)
	case "events", "news":
		return resetEvents(ctx, store)
	case "captures":
		return resetCaptures(ctx, store)
	default:
		return fmt.Errorf("unknown --only subsystem %q (valid: all, battlefields, events, captures)", only)
	}
}

// resetBattlefields re-initializes the stored battlefield occupation to the canonical starting layout:
// every area's maps at their default per-nation occupation (100% owned by the area's default nation) with
// each battlefield's capacity set to its known capture points (or 25000), divided by downscale (see
// seedBattlefields). DESTRUCTIVE -- discards war progression from post-mission (1214) battle reports.
func resetBattlefields(ctx context.Context, store *persistence.Store, downscale int32) error {
	coll := store.Collection(battlefieldsCollection)
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "areaId", Value: 1}, {Key: "mapId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	if _, err := coll.DeleteMany(ctx, bson.M{}); err != nil {
		return err
	}
	docs := seedBattlefields(downscale)
	anyDocs := make([]any, len(docs))
	for i, d := range docs {
		anyDocs[i] = d
	}
	if _, err := coll.InsertMany(ctx, anyDocs); err != nil {
		return err
	}
	// Reset per-nation season state. Fresh battlefields put every nation back at 100% of its own capital, so
	// no nation is dissolved: clear deadFlag + hqLostTo (a nation dissolved at the end of the previous season
	// must start the new one ALIVE) and zero battleCount (the capture-lock clock -- a stale count would leave
	// the unlock thresholds reading from an arbitrary baseline).
	if _, err := store.Collection(nationsCollection).UpdateMany(ctx, bson.M{},
		bson.M{"$set": bson.M{"battleCount": int32(0), "deadFlag": int32(0)}, "$unset": bson.M{"hqLostTo": ""}}); err != nil {
		return err
	}
	// Clear the per-area fierce-battle tallies so the new war starts with no fierce flags.
	_, err := store.Collection(areaStatsCollection).DeleteMany(ctx, bson.M{})
	return err
}

// resetEvents clears the world-event / news feed and seeds one "world briefing" event, so stale stories
// from the previous war don't carry over and the board is never empty (the title rejects a zero-count
// news reply). Fields match server.EventRecord (row 75 = "War Breaks Out In Neroimus").
func resetEvents(ctx context.Context, store *persistence.Store) error {
	events := store.Collection(eventsCollection)
	if _, err := events.DeleteMany(ctx, bson.M{}); err != nil {
		return err
	}
	_, err := events.InsertOne(ctx, bson.M{
		"createdAt":  time.Now().Unix(),
		"templateId": int32(75), // param ROW 75 = "War Breaks Out In Neroimus"
		"slot1":      "",
		"slot2":      "",
	})
	return err
}

// resetCaptures clears the per-squad capture-points ledger (backs the |s2= "most-involved squad").
func resetCaptures(ctx context.Context, store *persistence.Store) error {
	_, err := store.Collection(capturesCollection).DeleteMany(ctx, bson.M{})
	return err
}
