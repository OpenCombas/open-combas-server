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
	worldReadTimeout       = 3 * time.Second
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
}

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
}

// WorldRepository reads/writes the war-state collections on the shared MongoDB.
type WorldRepository struct {
	battlefields *mongo.Collection
	nations      *mongo.Collection
	events       *mongo.Collection
	captures     *mongo.Collection
}

func NewWorldRepository(store *persistence.Store) *WorldRepository {
	return &WorldRepository{
		battlefields: store.Collection(battlefieldsCollection),
		nations:      store.Collection(nationsCollection),
		events:       store.Collection(eventsCollection),
		captures:     store.Collection(capturesCollection),
	}
}

// captureContribution accumulates one squad's capture points on one battlefield. It backs the |s2=
// "most-involved squad" placeholder in region/battlefield capture news events.
type captureContribution struct {
	AreaID int32  `bson:"areaId"`
	MapID  int32  `bson:"mapId"`
	Squad  string `bson:"squad"`
	Points int64  `bson:"points"`
}

// CreditCapture adds a squad's capture points (== battle OccDelta) on a battlefield. No-op for an empty
// squad or non-positive points.
func (r *WorldRepository) CreditCapture(ctx context.Context, areaID, mapID int32, squad string, points int32) error {
	if squad == "" || points <= 0 {
		return nil
	}
	_, err := r.captures.UpdateOne(ctx,
		bson.M{"areaId": areaID, "mapId": mapID, "squad": squad},
		bson.M{"$inc": bson.M{"points": int64(points)}},
		options.UpdateOne().SetUpsert(true))
	return err
}

// TopSquadForBattlefield returns the squad with the most accumulated capture points on a battlefield
// (empty string if none).
func (r *WorldRepository) TopSquadForBattlefield(ctx context.Context, areaID, mapID int32) (string, error) {
	var c captureContribution
	err := r.captures.FindOne(ctx,
		bson.M{"areaId": areaID, "mapId": mapID},
		options.FindOne().SetSort(bson.D{{Key: "points", Value: -1}})).Decode(&c)
	if err == mongo.ErrNoDocuments {
		return "", nil
	}
	return c.Squad, err
}

// TopSquadForRegion returns the squad with the most capture points summed across an area's battlefields.
func (r *WorldRepository) TopSquadForRegion(ctx context.Context, areaID int32) (string, error) {
	cur, err := r.captures.Find(ctx, bson.M{"areaId": areaID})
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

// ApplyBattleOccupation moves occupation on one battlefield after a battle: the winning nation gains
// `delta` (capped at capacity), the losing nation loses `delta` (floored at 0). Atomic clamp via a
// pipeline update. A no-op when the winner nation is unknown or delta is 0.
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
	return AreaMapRecord{
		MapID:              int16(b.MapID),
		ControllingFaction: nationChar(leader),
		Capacity:           b.Capacity,
		ControlLevel:       level,
		OccupationPoints:   b.StrategicValue,
		InvasionA:          b.OccA,
		InvasionB:          b.OccB,
		InvasionC:          b.OccC,
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
