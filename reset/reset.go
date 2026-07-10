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
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	battlefieldsCollection = "battlefields"
	eventsCollection       = "events"
	capturesCollection     = "captureContributions"

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
	0, // index 0 unused (areas are 1-based)
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

// capturePoints overrides the default capacity for battlefields whose capture points are known. Keyed by
// [areaID, mapID]; every battlefield not listed here uses defaultCapturePoints (25000). Only the non-zero
// entries from known_occ_points.png are recorded (a 0 there means "unknown" -> default).
//
// Transcribed from known_occ_points.png; every entry's (areaID, mapID) was verified against the
// battlefield names/order in chromehounds_battlefields_areas.csv.
var capturePoints = map[[2]int32]int32{
	{5, 1}:  30000, // Zavoywko Dam 1
	{5, 3}:  30000, // Zavoywko Dam 2
	{6, 2}:  25000, // Base Ruins Perimeter
	{7, 2}:  10000, // Northern Walls
	{9, 2}:  10000, // Loxton Gorge
	{9, 4}:  10000, // Edina Mine Ruins
	{10, 3}: 30000, // Southworth Highway
	{10, 4}: 30000, // Village Ruin
	{13, 3}: 30000, // West Industrial Zone
	{14, 1}: 40000, // North Stanthorpe Bay
	{15, 2}: 25000, // East Cecil Plains
	{16, 2}: 30000, // East Lake Orlovka
	{17, 1}: 20000, // Savonovo
	{17, 4}: 10000, // East Salma Woods
	{18, 1}: 38000, // North Cemo Oil Field
	{18, 3}: 32000, // North Dinar Plains
	{18, 4}: 32000, // South Cemo Oil Field
	{19, 1}: 30000, // West Bayazit River
	{19, 2}: 20000, // East Bayazit River
	{19, 3}: 30000, // Biraal Water Plant
	{20, 1}: 10000, // Ruins
	{20, 2}: 20000, // Old Berri Coal Mine
	{20, 3}: 10000, // North Cressy Desert
	{21, 2}: 28000, // Bas Bar
	{21, 3}: 28000, // North Durama Desert
	{21, 4}: 28000, // South Durama Desert
	{22, 4}: 28000, // South Mine
}

// capacityFor returns a battlefield's occupation-pool capacity: its known capture points, else the default.
func capacityFor(area, mapID int32) int32 {
	if p, ok := capturePoints[[2]int32{area, mapID}]; ok {
		return p
	}
	return defaultCapturePoints
}

// areaDotTotal is the strategic-value ("orange dots") an area distributes across its battlefields: every
// area's dots sum to 5.
const areaDotTotal = 5

// dotOverride gives an explicit per-battlefield dot count (indexed by mapID-1) for areas whose real
// distribution we know. Areas not listed use the default: 1 dot each, with the remaining (5-count) dots
// added from map 1 upward.
//
// TODO(reset): the totals are correct -- every area sums to 5, and the 3-battlefield split is a confirmed
// 2+2+1 (operator). What's still provisional is WHICH battlefield carries the extra dot(s): only Tajin
// (battlefield_info screenshot) and Braidwood (operator) are known; the rest default to filling from map 1
// upward. Replace entries here once the per-area distribution is recovered from the archive.
var dotOverride = map[int32][]int32{
	10: {1, 1, 1, 2}, // Tajin: N/S Village Ruin + Southworth = 1, Village Ruin (map 4) = 2
	12: {2, 1, 1, 1}, // Braidwood: Cobar (map 1) = 2, N/S Bath Plains + Ebus Lake = 1
}

// areaDots returns the strategic-value (dots) for each battlefield in an area, summing to areaDotTotal.
func areaDots(area, count int32) []int32 {
	if ov, ok := dotOverride[area]; ok && int32(len(ov)) == count {
		return ov
	}
	dots := make([]int32, count)
	for i := range dots {
		dots[i] = 1
	}
	for i, extra := int32(0), areaDotTotal-count; extra > 0 && i < count; i, extra = i+1, extra-1 {
		dots[i]++
	}
	return dots
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
		dots := areaDots(area, count)
		for mapID := int32(1); mapID <= count; mapID++ {
			cap := capacityFor(area, mapID) / downscale
			if cap < 1 {
				cap = 1
			}
			bf := battlefield{AreaID: area, MapID: mapID, Capacity: cap, StrategicValue: dots[mapID-1]}
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

// BattlefieldReset re-initializes the stored battlefield occupation to the canonical starting layout:
// every area's maps at their default per-nation occupation (100% owned by the area's default nation) with
// each battlefield's capacity set to its known capture points (or 25000), divided by downscale (see
// seedBattlefields). This is DESTRUCTIVE -- it discards any war progression accumulated from post-mission
// (1214) battle reports.
func BattlefieldReset(ctx context.Context, store *persistence.Store, downscale int32) error {
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

	// A reset is a new season: clear the accumulated world-event / news feed so stale "nation dissolved"
	// stories from the previous war don't carry over, then seed one "world briefing" event. The news
	// board must never be empty -- the title rejects a zero-count news reply as "communication failed".
	// Ids match server.initEvent* (header text 14046, body text 15148); field names match EventRecord.
	events := store.Collection(eventsCollection)
	if _, err := events.DeleteMany(ctx, bson.M{}); err != nil {
		return err
	}
	if _, err := events.InsertOne(ctx, bson.M{
		"createdAt":  time.Now().Unix(),
		"templateId": int32(75), // param ROW 75 = "War Breaks Out In Neroimus" (header 14046 + body 15148)
		"slot1":      "",        // no placeholders in this story
		"slot2":      "",
	}); err != nil {
		return err
	}

	// Also clear the per-squad capture-points ledger (backs the |s2= "most-involved squad" in capture
	// events) so a new season starts with no accumulated contributions.
	if _, err := store.Collection(capturesCollection).DeleteMany(ctx, bson.M{}); err != nil {
		return err
	}
	return nil
}
