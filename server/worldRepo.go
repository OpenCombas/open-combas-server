package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Phase 1 persistence for the world model. The combas server owns two MongoDB collections holding the
// MUTABLE war state that was previously hard-coded in worldData.go:
//
//   battlefields - one doc per (area, battlefield): the three nations' occupation. This is the source of
//                  truth; controlling faction / control level / area-level control are DERIVED from it (so
//                  the post-mission 1214 ingest just mutates occA/occB/occC).
//   nations      - one doc per faction: the economy/population figures the World Situation screen shows.
//
// Everything else (the wire structs, the derivation math) stays in Go, and the collections are SEEDED
// from the existing static model, so the served bytes are identical until a battle report changes them.
// See project_combas_server_protocol memory.

const (
	battlefieldsCollection = "battlefields"
	nationsCollection      = "nations"
	eventsCollection       = "events"
	capturesCollection     = "captureContributions"
	areaStatsCollection    = "areaBattleStats"
	worldReadTimeout       = 3 * time.Second

	// fierceBattlePvpPercent is the PvP share of an area's battle reports above which its "fierce battle"
	// flag (激戦エリアフラグ) is set. The share resets when the area flips owner.
	fierceBattlePvpPercent = 30
)

// EventRecord is one world event, stored in the `events` collection and rendered as a WORLD_NEWS item
// (worldNewsServer). Created when the war state transitions (e.g. a nation is eliminated).
//
// WIRE MODEL (reverse-engineered from WorldSituationInfoNewsParam.bin + RE news-render trace, 2026-07-10):
// TemplateID is the PARAM ROW id (short@0) -- the client looks it up in the .bin to get {header text id,
// 2 body text ids} and composes the whole story client-side (both nation names baked into the chosen row).
// So row 3 = Xeres/Tarakia->Morskoj dissolution, row 75 = "War Breaks Out". The placeholder tokens in
// those texts (|<letter><digit>=) read RAW STRINGS from the entry's 66-byte C66 field, which is TWO 33-byte
// slots: DIGIT-1 tokens (|a1=/|A1=/|B1=/|c1=/|p1=) read slot1 (C66[0..32]); DIGIT-2 tokens (|s2=/|a2=)
// read slot2 (C66[33..65]). Names must be PRE-RESOLVED server-side (see worldNames.go), not sent as ids.
// The wire short@68 is only the header's cosmetic date number (we send 0). See worldNewsServer newsEntryFromEvent.
type EventRecord struct {
	CreatedAt  int64  `bson:"createdAt"`  // Unix seconds; drives news ordering (newest first) + the header date
	TemplateID int32  `bson:"templateId"` // PARAM ROW id into WorldSituationInfoNewsParam.bin (NOT a raw text id)
	Slot1      string `bson:"slot1"`      // C66[0..32]  -- digit-1 placeholder value (region/battlefield name, ...)
	Slot2      string `bson:"slot2"`      // C66[33..65] -- digit-2 placeholder value (squad name, ...)
}

// Battlefield is the stored occupation state for one battlefield within an area.
type Battlefield struct {
	AreaID         int32 `bson:"areaId"`
	MapID          int32 `bson:"mapId"` // 1-based index within the area
	Capacity       int32 `bson:"capacity"`
	StrategicValue int32 `bson:"strategicValue"` // occupation points (orange dots)
	OccA           int32 `bson:"occA"`           // Tarakia occupation level
	OccB           int32 `bson:"occB"`           // Morskoj
	OccC           int32 `bson:"occC"`           // Sal Kar
	// Capture lock: a battlefield/area flips 100% to the winner on a change-of-hands and locks out the
	// defeated nation (no missions there) until that nation has fought UnlockBattleThreshold other
	// battles anywhere. This models the retail winner-takes-all mechanic that prevents occupation
	// tug-of-war from dragging a season out. Unset (via omitempty) == contestable.
	Locked         bool   `bson:"locked,omitempty"`
	DefeatedNation string `bson:"defeatedNation,omitempty"` // "A"/"B"/"C" locked out of this battlefield
	UnlockAtBattle int32  `bson:"unlockAtBattle,omitempty"` // DefeatedNation battleCount at which it reopens
}

// UnlockBattleThreshold is how many other battles the defeated nation must fight (anywhere, win or
// lose) before a battlefield it lost reopens for missions.
const UnlockBattleThreshold = 10

// NationRecord is the stored per-faction economy/state shown on the World Situation screen. It mirrors
// the meaningful NationData fields (the wire struct has pad bytes that don't belong in storage).
type NationRecord struct {
	CountryCode       string  `bson:"countryCode"` // "A"/"B"/"C"
	TotalIncome       int32   `bson:"totalIncome"`
	FixedIncome       int32   `bson:"fixedIncome"`
	NumberOfAreas     int32   `bson:"numberOfAreas"`
	Field16           float32 `bson:"field16"`
	ExchangeRate      int32   `bson:"exchangeRate"`
	Population        int32   `bson:"population"`
	NumberOfSoldiers  int32   `bson:"numberOfSoldiers"`
	NumberOfPlayers   int32   `bson:"numberOfPlayers"`
	ResearchLevel     int32   `bson:"researchLevel"`
	ResearchBudget    int32   `bson:"researchBudget"`
	MaintenanceBudget int32   `bson:"maintenanceBudget"`
	MilitaryBudget    int32   `bson:"militaryBudget"`
	PriceIndex        float32 `bson:"priceIndex"`
	PresidentID       int32   `bson:"presidentId"`
	Unknown57         int32   `bson:"unknown57"`
	DeadFlag          int32   `bson:"deadFlag"`
	BattleCount       int32   `bson:"battleCount,omitempty"` // battles this nation has fought; drives the capture-lock clock
	// HQLostTo is set to the nation ('A'/'B'/'C') that captured this nation's capital when its HQ falls.
	// While set the nation is "dissolved": it may only launch missions on its own HQ area until it
	// recaptures it (which fires a revival event and clears this). "" == holding its capital.
	HQLostTo string `bson:"hqLostTo,omitempty"`
}

// WorldRepository reads/writes the war-state collections on the shared MongoDB.
type WorldRepository struct {
	battlefields *mongo.Collection
	nations      *mongo.Collection
	events       *mongo.Collection
	captures     *mongo.Collection
	areaStats    *mongo.Collection
	maintenance  *mongo.Collection
}

func NewWorldRepository(store *persistence.Store) *WorldRepository {
	return &WorldRepository{
		battlefields: store.Collection(battlefieldsCollection),
		nations:      store.Collection(nationsCollection),
		events:       store.Collection(eventsCollection),
		captures:     store.Collection(capturesCollection),
		areaStats:    store.Collection(areaStatsCollection),
		maintenance:  store.Collection(maintenanceCollection),
	}
}

// captureContribution accumulates one squad's capture points on one battlefield. It backs the |s2=
// "most-involved squad" placeholder in region/battlefield capture news events. Nation ("A"/"B"/"C") is the
// side the squad fought FOR (the battle winner's nation), so a capture event can surface the top squad of the
// CAPTURING nation rather than the all-time leader (which is often a rival that won this battlefield earlier).
type captureContribution struct {
	AreaID int32  `bson:"areaId"`
	MapID  int32  `bson:"mapId"`
	Squad  string `bson:"squad"`
	Nation string `bson:"nation,omitempty"`
	Points int64  `bson:"points"`
}

// CreditCapture adds a squad's capture points (== battle OccDelta) on a battlefield for the nation it fought
// for. No-op for an empty squad or non-positive points.
func (r *WorldRepository) CreditCapture(ctx context.Context, areaID, mapID int32, squad, nation string, points int32) error {
	if squad == "" || points <= 0 {
		return nil
	}
	_, err := r.captures.UpdateOne(ctx,
		bson.M{"areaId": areaID, "mapId": mapID, "squad": squad},
		bson.M{"$inc": bson.M{"points": int64(points)}, "$set": bson.M{"nation": nation}},
		options.UpdateOne().SetUpsert(true))
	return err
}

// TopSquadForBattlefield returns the given NATION's squad with the most accumulated capture points on a
// battlefield (empty string if none). Filtering by nation keeps the |s2= squad on the capturing side.
func (r *WorldRepository) TopSquadForBattlefield(ctx context.Context, areaID, mapID int32, nation string) (string, error) {
	var c captureContribution
	err := r.captures.FindOne(ctx,
		bson.M{"areaId": areaID, "mapId": mapID, "nation": nation},
		options.FindOne().SetSort(bson.D{{Key: "points", Value: -1}})).Decode(&c)
	if err == mongo.ErrNoDocuments {
		return "", nil
	}
	return c.Squad, err
}

// TopSquadForRegion returns the given NATION's squad with the most capture points summed across an area's
// battlefields.
func (r *WorldRepository) TopSquadForRegion(ctx context.Context, areaID int32, nation string) (string, error) {
	cur, err := r.captures.Find(ctx, bson.M{"areaId": areaID, "nation": nation})
	if err != nil {
		return "", err
	}
	var docs []captureContribution
	if err := cur.All(ctx, &docs); err != nil {
		return "", err
	}
	total := map[string]int64{}
	for _, d := range docs {
		total[d.Squad] += d.Points
	}
	best, bestPts := "", int64(0)
	for s, p := range total {
		if p > bestPts {
			best, bestPts = s, p
		}
	}
	return best, nil
}

// AreaAndBFLead returns the current lead nation ('A'/'B'/'C') of an area (which nation controls the most
// of its battlefields) and of one specific battlefield within it. Used before/after a battle to detect a
// region or battlefield capture (a change of lead). Returns 0 for an unknown/empty area.
func (r *WorldRepository) AreaAndBFLead(ctx context.Context, areaID, mapID int32) (areaOwner, bfLead byte, err error) {
	cur, err := r.battlefields.Find(ctx, bson.M{"areaId": areaID})
	if err != nil {
		return 0, 0, err
	}
	var bfs []Battlefield
	if err := cur.All(ctx, &bfs); err != nil {
		return 0, 0, err
	}
	if len(bfs) == 0 {
		return 0, 0, nil
	}
	areaOwner, _, _, _ = areaSummaryFrom(bfs)
	for _, b := range bfs {
		if b.MapID == mapID {
			idx, _ := leadFaction(b.OccA, b.OccB, b.OccC)
			bfLead = nationChar(idx)
		}
	}
	return areaOwner, bfLead, nil
}

// RecordEvent appends one world event (rendered later as a WORLD_NEWS item).
func (r *WorldRepository) RecordEvent(ctx context.Context, ev EventRecord) error {
	_, err := r.events.InsertOne(ctx, ev)
	return err
}

// RecentEvents returns up to limit world events, newest first (by createdAt, then insertion order).
func (r *WorldRepository) RecentEvents(ctx context.Context, limit int) ([]EventRecord, error) {
	return r.RecentEventsPage(ctx, 0, limit)
}

// RecentEventsPage returns one page of world events, newest first: skip the newest `skip`, then take up
// to `limit`. Backs the WORLD_NEWS pager (the client requests page 1, page 2, ... and appends them).
func (r *WorldRepository) RecentEventsPage(ctx context.Context, skip, limit int) ([]EventRecord, error) {
	cur, err := r.events.Find(ctx, bson.M{},
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).
			SetSkip(int64(skip)).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var out []EventRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SeededBriefing returns the reset-seeded "War Breaks Out" briefing (the oldest row-initEventRow event),
// or (nil, nil) if none. Its CreatedAt is the season-start time; the news server serves it (rather than a
// freshly-stamped briefing) as the empty-category fallback so the client doesn't re-flash it every query.
func (r *WorldRepository) SeededBriefing(ctx context.Context) (*EventRecord, error) {
	var ev EventRecord
	err := r.events.FindOne(ctx,
		bson.M{"templateId": int32(initEventRow)},
		options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: 1}})).Decode(&ev)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// NationAreaCounts returns how many areas each nation currently controls, keyed by nation char 'A'/'B'/'C'.
// An area is controlled by the nation that leads the most of its battlefields (see areaSummaryFrom).
func (r *WorldRepository) NationAreaCounts(ctx context.Context) (map[byte]int, error) {
	grouped, err := r.BattlefieldsGrouped(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[byte]int{'A': 0, 'B': 0, 'C': 0}
	for _, bfs := range grouped {
		if len(bfs) == 0 {
			continue
		}
		owner, _, _, _ := areaSummaryFrom(bfs)
		counts[owner]++
	}
	return counts, nil
}

// EnsureSchema creates the indexes and seeds the nations collection from the static model if empty.
//
// It deliberately does NOT seed battlefields: initializing the battlefield war state is an explicit,
// destructive operation owned by the reset tool (package reset / cmd/reset), not something the server does
// on boot. Until a reset has run, the battlefields collection is empty and the world/area servers fall
// back to the static model, so the served bytes are unchanged. Nations still seed here (no reset for them
// yet). Seeding is idempotent: existing (already-mutated) state is never overwritten.
func (r *WorldRepository) EnsureSchema(ctx context.Context) error {
	if _, err := r.battlefields.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "areaId", Value: 1}, {Key: "mapId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	if _, err := r.nations.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "countryCode", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	// events: sorted by createdAt for the news feed (RecentEvents).
	if _, err := r.events.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "createdAt", Value: -1}},
	}); err != nil {
		return err
	}
	// captureContributions: one doc per (area, map, squad) -- unique so CreditCapture's upsert increments.
	if _, err := r.captures.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "areaId", Value: 1}, {Key: "mapId", Value: 1}, {Key: "squad", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	// areaBattleStats: one doc per area -- unique so CreditAreaBattle's upsert increments the tally.
	if _, err := r.areaStats.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "areaId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}

	// Seed-if-empty (idempotent, NON-destructive -- runs on every boot, never wipes existing war state):
	// nations economy, and one briefing news event so a fresh DB / a newly-added events feature has a
	// non-empty board WITHOUT needing a full world reset. Existing rows are left untouched.
	if err := seedIfEmpty(ctx, r.nations, toAny(seedNations())); err != nil {
		return err
	}
	return seedIfEmpty(ctx, r.events, []any{briefingEvent(time.Now().Unix())})
}

func (r *WorldRepository) BattlefieldsByArea(ctx context.Context, areaID byte) ([]Battlefield, error) {
	cur, err := r.battlefields.Find(ctx, bson.M{"areaId": int32(areaID)},
		options.Find().SetSort(bson.D{{Key: "mapId", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var out []Battlefield
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BattlefieldByAreaMap returns the single battlefield (area, map), or (nil, nil) if none exists.
func (r *WorldRepository) BattlefieldByAreaMap(ctx context.Context, areaID, mapID byte) (*Battlefield, error) {
	var b Battlefield
	err := r.battlefields.FindOne(ctx, bson.M{"areaId": int32(areaID), "mapId": int32(mapID)}).Decode(&b)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// occField maps a nation char ('A'/'B'/'C') to its occupation field name; "" for an unknown nation.
func occField(nation byte) string {
	switch nation {
	case 'A':
		return "occA"
	case 'B':
		return "occB"
	case 'C':
		return "occC"
	}
	return ""
}

// bfLead returns the nation ('A'/'B'/'C') currently leading a battlefield's occupation, or 0 if none has
// any. During the accumulation phase a battlefield can be contested (both nations hold some occupation);
// once captured it snaps to 100% for one nation and locks.
func bfLead(b Battlefield) byte {
	idx, level := leadFaction(b.OccA, b.OccB, b.OccC)
	if level == 0 {
		return 0
	}
	return nationChar(idx)
}

// occIndexOf maps a nation char to its occ-array index (A=0/B=1/C=2), -1 for unknown.
func occIndexOf(nation byte) int {
	switch nation {
	case 'A':
		return 0
	case 'B':
		return 1
	case 'C':
		return 2
	}
	return -1
}

// leadAfterDelta returns the battlefield's leading nation AFTER a mission's occupation shift (winner
// +delta capped at capacity, loser -delta floored at 0) -- computed WITHOUT writing. applyBattle uses it
// to detect the crossover: a battlefield is captured only when this post-shift lead differs from the prior
// holder (the attacker has overtaken), not merely because someone won a mission there.
func leadAfterDelta(bf Battlefield, winner, loser byte, delta int32) byte {
	occ := [3]int32{bf.OccA, bf.OccB, bf.OccC}
	if wi := occIndexOf(winner); wi >= 0 {
		if occ[wi] += delta; occ[wi] > bf.Capacity {
			occ[wi] = bf.Capacity
		}
	}
	if li := occIndexOf(loser); li >= 0 {
		if occ[li] -= delta; occ[li] < 0 {
			occ[li] = 0
		}
	}
	if idx, level := leadFaction(occ[0], occ[1], occ[2]); level > 0 {
		return nationChar(idx)
	}
	return 0
}

// ApplyBattleOccupation shifts occupation on one battlefield after a NON-capturing mission: the winner
// gains `delta` (capped at capacity), the loser loses `delta` (floored at 0). This is the accumulation
// phase -- the battlefield only flips 100% + locks once the attacker overtakes the holder (see
// applyBattle). Atomic clamp via a pipeline update; no-op for an unknown winner or 0 delta.
func (r *WorldRepository) ApplyBattleOccupation(ctx context.Context, areaID, mapID, winnerNation, loserNation byte, delta int32) error {
	winField, loseField := occField(winnerNation), occField(loserNation)
	if winField == "" || delta == 0 {
		return nil
	}
	set := bson.M{
		winField: bson.M{"$min": bson.A{bson.M{"$add": bson.A{"$" + winField, delta}}, "$capacity"}},
	}
	if loseField != "" && loseField != winField {
		set[loseField] = bson.M{"$max": bson.A{bson.M{"$subtract": bson.A{"$" + loseField, delta}}, int32(0)}}
	}
	_, err := r.battlefields.UpdateOne(ctx,
		bson.M{"areaId": int32(areaID), "mapId": int32(mapID)},
		mongo.Pipeline{{{Key: "$set", Value: set}}},
	)
	return err
}

// captureSet builds the pipeline $set that flips a battlefield 100% to `winner` and locks out
// `defeated` until that nation reaches `unlockAt` battles. The winner's occ field is set to the
// battlefield's own capacity (a "$capacity" field reference), the other two to zero.
func captureSet(winner, defeated byte, unlockAt int32) bson.M {
	occ := func(n byte) interface{} {
		if n == winner {
			return "$capacity"
		}
		return int32(0)
	}
	return bson.M{
		"occA": occ('A'), "occB": occ('B'), "occC": occ('C'),
		"locked": true, "defeatedNation": string(defeated), "unlockAtBattle": unlockAt,
	}
}

// CaptureBattlefield flips one battlefield 100% to the winning nation and locks out the defeated
// nation until it has fought `unlockAt` battles. Used when a battle changes a battlefield's holder.
func (r *WorldRepository) CaptureBattlefield(ctx context.Context, areaID, mapID int32, winner, defeated byte, unlockAt int32) error {
	if occField(winner) == "" {
		return nil
	}
	_, err := r.battlefields.UpdateOne(ctx,
		bson.M{"areaId": areaID, "mapId": mapID},
		mongo.Pipeline{{{Key: "$set", Value: captureSet(winner, defeated, unlockAt)}}},
	)
	return err
}

// FlipAndLockArea flips every battlefield in an area 100% to the winning nation and locks the whole
// area against the defeated (surrendering) nation. Used when a battlefield capture flips the area
// owner: the retail behaviour is that the loser surrenders the entire area, not just the fought map.
func (r *WorldRepository) FlipAndLockArea(ctx context.Context, areaID int32, winner, defeated byte, unlockAt int32) error {
	if occField(winner) == "" {
		return nil
	}
	_, err := r.battlefields.UpdateMany(ctx,
		bson.M{"areaId": areaID},
		mongo.Pipeline{{{Key: "$set", Value: captureSet(winner, defeated, unlockAt)}}},
	)
	return err
}

// UnlockExpiredBattlefields reopens every battlefield locked against `nation` whose unlock threshold
// has been reached (that nation's battleCount is now >= unlockAtBattle). Clears the lock fields.
func (r *WorldRepository) UnlockExpiredBattlefields(ctx context.Context, nation byte, count int32) error {
	_, err := r.battlefields.UpdateMany(ctx,
		bson.M{"defeatedNation": string(nation), "locked": true, "unlockAtBattle": bson.M{"$lte": count}},
		bson.M{"$set": bson.M{"locked": false}, "$unset": bson.M{"defeatedNation": "", "unlockAtBattle": ""}},
	)
	return err
}

// IncrementBattleCount bumps a nation's fought-battle counter and returns the new value. Returns
// (0, nil) for an unknown nation. This counter drives the capture-lock clock (UnlockBattleThreshold).
func (r *WorldRepository) IncrementBattleCount(ctx context.Context, nation byte) (int32, error) {
	var rec NationRecord
	err := r.nations.FindOneAndUpdate(ctx,
		bson.M{"countryCode": string(nation)},
		bson.M{"$inc": bson.M{"battleCount": int32(1)}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&rec)
	if err == mongo.ErrNoDocuments {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rec.BattleCount, nil
}

// NationBattleCount reads a nation's current fought-battle counter (0 if unknown).
func (r *WorldRepository) NationBattleCount(ctx context.Context, nation byte) (int32, error) {
	var rec NationRecord
	err := r.nations.FindOne(ctx, bson.M{"countryCode": string(nation)}).Decode(&rec)
	if err == mongo.ErrNoDocuments {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rec.BattleCount, nil
}

// SetNationHQLost marks a nation as dissolved (its capital fell to captor). ClearNationHQLost reverses
// it on revival. NationHQLostTo reads the captor char (0 if the nation is not dissolved / unknown).
//
// These also drive DeadFlag: the wire NationData.DeadFlag is the client's "defeated nation" signal
// (map pulse for its members, squad defection, HQ-only sorties), so a dissolved nation sets it and a
// revived nation clears it -- keeping the DB deadFlag in lockstep with the dissolved state.
func (r *WorldRepository) SetNationHQLost(ctx context.Context, nation, captor byte) error {
	_, err := r.nations.UpdateOne(ctx,
		bson.M{"countryCode": string(nation)},
		bson.M{"$set": bson.M{"hqLostTo": string(captor), "deadFlag": int32(1)}})
	return err
}

func (r *WorldRepository) ClearNationHQLost(ctx context.Context, nation byte) error {
	_, err := r.nations.UpdateOne(ctx,
		bson.M{"countryCode": string(nation)},
		bson.M{"$set": bson.M{"deadFlag": int32(0)}, "$unset": bson.M{"hqLostTo": ""}})
	return err
}

func (r *WorldRepository) NationHQLostTo(ctx context.Context, nation byte) (byte, error) {
	var rec NationRecord
	err := r.nations.FindOne(ctx, bson.M{"countryCode": string(nation)}).Decode(&rec)
	if err == mongo.ErrNoDocuments {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if rec.HQLostTo == "" {
		return 0, nil
	}
	return rec.HQLostTo[0], nil
}

// FlipAreaUnlocked flips every battlefield in an area 100% to `winner` and clears any capture lock,
// leaving the area contestable again. Used for HQ falls: the fallen capital and the dissolved nation's
// cascaded areas move to the captor but must stay open (the dissolved nation has to be able to attack
// its HQ to revive, and other nations may contest the captor's new holdings normally).
func (r *WorldRepository) FlipAreaUnlocked(ctx context.Context, areaID int32, winner byte) error {
	if occField(winner) == "" {
		return nil
	}
	occ := func(n byte) interface{} {
		if n == winner {
			return "$capacity"
		}
		return int32(0)
	}
	_, err := r.battlefields.UpdateMany(ctx,
		bson.M{"areaId": areaID},
		mongo.Pipeline{
			{{Key: "$set", Value: bson.M{"occA": occ('A'), "occB": occ('B'), "occC": occ('C'), "locked": false}}},
			{{Key: "$unset", Value: bson.A{"defeatedNation", "unlockAtBattle"}}},
		})
	return err
}

// AreasOwnedBy returns the area ids a nation currently controls (owner per areaSummaryFrom).
func (r *WorldRepository) AreasOwnedBy(ctx context.Context, nation byte) ([]int32, error) {
	grouped, err := r.BattlefieldsGrouped(ctx)
	if err != nil {
		return nil, err
	}
	var owned []int32
	for areaID, bfs := range grouped {
		if owner, _, _, _ := areaSummaryFrom(bfs); owner == nation {
			owned = append(owned, int32(areaID))
		}
	}
	return owned, nil
}

// BattlefieldsGrouped returns every battlefield keyed by area id (sorted by map id within each area).
func (r *WorldRepository) BattlefieldsGrouped(ctx context.Context) (map[byte][]Battlefield, error) {
	cur, err := r.battlefields.Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "areaId", Value: 1}, {Key: "mapId", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var all []Battlefield
	if err := cur.All(ctx, &all); err != nil {
		return nil, err
	}
	grouped := make(map[byte][]Battlefield, len(all))
	for _, b := range all {
		grouped[byte(b.AreaID)] = append(grouped[byte(b.AreaID)], b)
	}
	return grouped, nil
}

// CreditNationDonation applies one world-screen donation to a nation's totalIncome ("Total Revenue"),
// returning the status byte the client expects (parser sub_823BDFB0):
//
//	'1' Complete       - credited
//	'2' Country is Dead - target nation eliminated (no credit)
//	'3' Acceptance end  - unknown nation / not accepting (no credit)
//
// The credit is clamped to the int32 ceiling of the wire field so repeated donations can't wrap it
// negative. The write is guarded on deadFlag == 0 so a nation that dies between the read and the write
// is not credited.
func (r *WorldRepository) CreditNationDonation(ctx context.Context, nation byte, amount int32) (byte, error) {
	code := string(nation)

	var rec NationRecord
	err := r.nations.FindOne(ctx, bson.M{"countryCode": code}).Decode(&rec)
	if err == mongo.ErrNoDocuments {
		return '3', nil // unknown nation -> not accepting
	}
	if err != nil {
		return 0, err
	}
	if rec.DeadFlag != 0 {
		return '2', nil // Country is Dead
	}

	res, err := r.nations.UpdateOne(ctx,
		bson.M{"countryCode": code, "deadFlag": int32(0)},
		mongo.Pipeline{{{Key: "$set", Value: bson.M{
			"totalIncome": bson.M{"$min": bson.A{
				bson.M{"$add": bson.A{"$totalIncome", amount}},
				int32(2147483647), // int32 ceiling of the wire NationData.TotalIncome field
			}},
		}}}},
	)
	if err != nil {
		return 0, err
	}
	if res.MatchedCount == 0 {
		return '2', nil // raced a nation death between the read and the guarded write
	}
	return '1', nil
}

// worldTotalAreas is the number of map areas the client divides by to render a nation's Control %
// (NumberOfAreas / worldTotalAreas -- e.g. 5/22 = 22.7%). See worldData.go (22 areas, ids 1..22).
const worldTotalAreas = 22

// controlAreasFromBattlefields maps live battlefield occupation onto each nation's Control-% field
// (NationData.NumberOfAreas), so the rendered Control % is that nation's share of the map's TOTAL
// occupation points rather than a static placeholder. Each battlefield contributes its StrategicValue
// (the "occupation points" / orange dots) to a nation in proportion to that nation's occupation of the
// battlefield (OccX / Capacity; OccA+OccB+OccC == Capacity). The resulting point-share is scaled onto
// the client's /worldTotalAreas denominator. Pure so it can be unit-tested without Mongo.
func controlAreasFromBattlefields(bfs []Battlefield) (a, b, c byte) {
	var ptsA, ptsB, ptsC, total float64
	for _, bf := range bfs {
		if bf.Capacity <= 0 || bf.StrategicValue <= 0 {
			continue // no capacity / no occupation points to attribute
		}
		sv := float64(bf.StrategicValue)
		total += sv
		ptsA += sv * float64(bf.OccA) / float64(bf.Capacity)
		ptsB += sv * float64(bf.OccB) / float64(bf.Capacity)
		ptsC += sv * float64(bf.OccC) / float64(bf.Capacity)
	}
	if total == 0 {
		return 0, 0, 0
	}
	toAreas := func(p float64) byte {
		v := int(p/total*worldTotalAreas + 0.5) // share -> nearest area unit on the /22 scale
		if v < 0 {
			v = 0
		}
		if v > worldTotalAreas {
			v = worldTotalAreas
		}
		return byte(v)
	}
	return toAreas(ptsA), toAreas(ptsB), toAreas(ptsC)
}

// NationControlAreas reads every battlefield and returns the per-nation Control-% field (NumberOfAreas)
// for A, B, C derived from live occupation. See controlAreasFromBattlefields.
func (r *WorldRepository) NationControlAreas(ctx context.Context) (byte, byte, byte, error) {
	cur, err := r.battlefields.Find(ctx, bson.M{})
	if err != nil {
		return 0, 0, 0, err
	}
	var bfs []Battlefield
	if err := cur.All(ctx, &bfs); err != nil {
		return 0, 0, 0, err
	}
	a, b, c := controlAreasFromBattlefields(bfs)
	return a, b, c, nil
}

// Nations returns the three faction records ordered A, B, C.
func (r *WorldRepository) Nations(ctx context.Context) ([]NationRecord, error) {
	cur, err := r.nations.Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "countryCode", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var out []NationRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func seedIfEmpty(ctx context.Context, coll *mongo.Collection, docs []any) error {
	n, err := coll.CountDocuments(ctx, bson.M{})
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = coll.InsertMany(ctx, docs)
	return err
}

func toAny[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// --- seed generators: project the static model (worldData.go / defaultNations) into stored docs ---

func seedBattlefields() []Battlefield {
	var out []Battlefield
	for areaID := byte(1); int(areaID) < len(areaBattlefieldCount); areaID++ {
		maps, count := areaBattlefields(areaID)
		for j := byte(0); j < count; j++ {
			m := maps[j]
			out = append(out, Battlefield{
				AreaID:         int32(areaID),
				MapID:          int32(m.MapID),
				Capacity:       m.Capacity,
				StrategicValue: m.OccupationPoints,
				OccA:           m.InvasionA,
				OccB:           m.InvasionB,
				OccC:           m.InvasionC,
			})
		}
	}
	return out
}

func seedNations() []NationRecord {
	nations := defaultNations()
	out := make([]NationRecord, len(nations))
	for i, n := range nations {
		out[i] = nationRecordFrom(n)
	}
	return out
}

// --- pure derivation: rebuild the wire records from stored state (identical to the static output) ---

// leadFaction returns the index (0=A,1=B,2=C) and level of the nation with the highest occupation.
func leadFaction(a, b, c int32) (idx int, level int32) {
	idx, level = 0, a
	if b > level {
		idx, level = 1, b
	}
	if c > level {
		idx, level = 2, c
	}
	return
}

func (b Battlefield) toAreaMapRecord() AreaMapRecord {
	leader, level := leadFaction(b.OccA, b.OccB, b.OccC)
	var lock byte
	if b.Locked {
		lock = 1 // マップロックフラグ: tells the client this battlefield is closed to missions
	}
	return AreaMapRecord{
		MapID:              int16(b.MapID),
		ControllingFaction: nationChar(leader),
		Capacity:           b.Capacity,
		ControlLevel:       level,
		OccupationPoints:   b.StrategicValue,
		InvasionA:          b.OccA,
		InvasionB:          b.OccB,
		InvasionC:          b.OccC,
		MapLockFlag:        lock,
	}
}

// areaMapRecordsFrom builds the per-area battlefield records (area-info / code 197) from stored docs.
func areaMapRecordsFrom(bfs []Battlefield) ([6]AreaMapRecord, byte) {
	var maps [6]AreaMapRecord
	sorted := append([]Battlefield(nil), bfs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MapID < sorted[j].MapID })
	n := 0
	for _, b := range sorted {
		if n >= len(maps) {
			break
		}
		maps[n] = b.toAreaMapRecord()
		n++
	}
	return maps, byte(n)
}

// areaBattleStat is one area's ingested-battle tally (total reports and the PvP subset), backing the
// fierce-battle flag.
type areaBattleStat struct {
	AreaID int32 `bson:"areaId"`
	Total  int64 `bson:"total"`
	PvP    int64 `bson:"pvp"`
}

// CreditAreaBattle records one ingested battle report for an area: total +1, and pvp +1 when the report
// is squad-vs-squad. Backs the fierce-battle flag (AreaFierceFlags). Upserts the area's tally.
func (r *WorldRepository) CreditAreaBattle(ctx context.Context, areaID int32, pvp bool) error {
	inc := bson.M{"total": int64(1)}
	if pvp {
		inc["pvp"] = int64(1)
	}
	_, err := r.areaStats.UpdateOne(ctx,
		bson.M{"areaId": areaID},
		bson.M{"$inc": inc},
		options.UpdateOne().SetUpsert(true))
	return err
}

// ResetAreaBattleStats clears an area's battle tally so its fierce-battle share restarts from zero. Called
// when the area flips owner (which also clears the flag).
func (r *WorldRepository) ResetAreaBattleStats(ctx context.Context, areaID int32) error {
	_, err := r.areaStats.DeleteOne(ctx, bson.M{"areaId": areaID})
	return err
}

// fierceFromCounts reports whether an area's PvP share exceeds the fierce-battle threshold.
func fierceFromCounts(total, pvp int64) bool {
	return total > 0 && pvp*100 > total*fierceBattlePvpPercent
}

// AreaBattleCounts returns an area's total and PvP battle-report tallies (0,0 if none). For display/tools.
func (r *WorldRepository) AreaBattleCounts(ctx context.Context, areaID int32) (total, pvp int64, err error) {
	var s areaBattleStat
	e := r.areaStats.FindOne(ctx, bson.M{"areaId": areaID}).Decode(&s)
	if e == mongo.ErrNoDocuments {
		return 0, 0, nil
	}
	if e != nil {
		return 0, 0, e
	}
	return s.Total, s.PvP, nil
}

// AreaFierce reports whether one area's fierce-battle flag is set (see AreaFierceFlags).
func (r *WorldRepository) AreaFierce(ctx context.Context, areaID int32) (bool, error) {
	var s areaBattleStat
	err := r.areaStats.FindOne(ctx, bson.M{"areaId": areaID}).Decode(&s)
	if err == mongo.ErrNoDocuments {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fierceFromCounts(s.Total, s.PvP), nil
}

// AreaFierceFlags returns, per area id, whether the fierce-battle flag is set: more than
// fierceBattlePvpPercent of that area's ingested battle reports have been PvP (squad vs squad).
func (r *WorldRepository) AreaFierceFlags(ctx context.Context) (map[int32]bool, error) {
	cur, err := r.areaStats.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var stats []areaBattleStat
	if err := cur.All(ctx, &stats); err != nil {
		return nil, err
	}
	flags := make(map[int32]bool, len(stats))
	for _, s := range stats {
		flags[s.AreaID] = fierceFromCounts(s.Total, s.PvP)
	}
	return flags, nil
}

// areaSummaryFrom derives area-level control (area-map / code 196) from stored battlefields: the owner
// leads the most battlefields, per-nation points are the averaged occupation. Mirrors areaControlSummary.
func areaSummaryFrom(bfs []Battlefield) (owner byte, pointsA, pointsB, pointsC int32) {
	if len(bfs) == 0 {
		return 'A', 0, 0, 0
	}
	// The area's per-nation OCCUPATION POINTS (the orange dots) = the strategic value of the battlefields
	// each nation CONTROLS. This is a DIFFERENT quantity from a battlefield's capture level (OccA/OccB/OccC):
	// capture level only decides who controls that battlefield and must NOT be surfaced to the area -- the raw
	// occupation magnitude (or its average) saturates the small 0-N dot display (e.g. full-capacity occ shows
	// every nation at max). occTotal is kept only as the owner tiebreak.
	var points, occTotal [3]int32
	for _, b := range bfs {
		occTotal[0] += b.OccA
		occTotal[1] += b.OccB
		occTotal[2] += b.OccC
		if idx, level := leadFaction(b.OccA, b.OccB, b.OccC); level > 0 {
			points[idx] += b.StrategicValue
		}
	}
	// Area owner = the nation holding the most occupation points (matches the dots), ties broken by total
	// occupation. (Was "most battlefields led", which on an even split silently defaulted to nation A.)
	best := 0
	for i := 1; i < 3; i++ {
		if points[i] > points[best] || (points[i] == points[best] && occTotal[i] > occTotal[best]) {
			best = i
		}
	}
	return nationChar(best), points[0], points[1], points[2]
}

// --- NationData <-> NationRecord conversions ---

func nationRecordFrom(n NationData) NationRecord {
	return NationRecord{
		CountryCode:       string(n.CountryCode),
		TotalIncome:       n.TotalIncome,
		FixedIncome:       n.FixedIncome,
		NumberOfAreas:     int32(n.NumberOfAreas),
		Field16:           n.Field16,
		ExchangeRate:      n.ExchangeRate,
		Population:        n.Population,
		NumberOfSoldiers:  n.NumberOfSoldiers,
		NumberOfPlayers:   n.NumberOfPlayers,
		ResearchLevel:     int32(n.ResearchLevel),
		ResearchBudget:    n.ResearchBudget,
		MaintenanceBudget: n.MaintenanceBudget,
		MilitaryBudget:    n.MilitaryBudget,
		PriceIndex:        n.PriceIndex,
		PresidentID:       int32(n.PresidentID),
		Unknown57:         int32(n.Unknown57),
		DeadFlag:          int32(n.DeadFlag),
	}
}

func (r NationRecord) toNationData() NationData {
	var code byte
	if len(r.CountryCode) > 0 {
		code = r.CountryCode[0]
	}
	return NationData{
		CountryCode:       code,
		TotalIncome:       r.TotalIncome,
		FixedIncome:       r.FixedIncome,
		NumberOfAreas:     byte(r.NumberOfAreas),
		Field16:           r.Field16,
		ExchangeRate:      r.ExchangeRate,
		Population:        r.Population,
		NumberOfSoldiers:  r.NumberOfSoldiers,
		NumberOfPlayers:   r.NumberOfPlayers,
		ResearchLevel:     uint16(r.ResearchLevel),
		ResearchBudget:    r.ResearchBudget,
		MaintenanceBudget: r.MaintenanceBudget,
		MilitaryBudget:    r.MilitaryBudget,
		PriceIndex:        r.PriceIndex,
		PresidentID:       byte(r.PresidentID),
		Unknown57:         byte(r.Unknown57),
		DeadFlag:          byte(r.DeadFlag),
	}
}
