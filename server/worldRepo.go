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
	worldReadTimeout       = 3 * time.Second
)

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
}

func NewWorldRepository(store *persistence.Store) *WorldRepository {
	return &WorldRepository{
		battlefields: store.Collection(battlefieldsCollection),
		nations:      store.Collection(nationsCollection),
	}
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

	return seedIfEmpty(ctx, r.nations, toAny(seedNations()))
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
	var sumA, sumB, sumC int32
	var leadCount [3]int
	for _, b := range bfs {
		sumA += b.OccA
		sumB += b.OccB
		sumC += b.OccC
		idx, _ := leadFaction(b.OccA, b.OccB, b.OccC)
		leadCount[idx]++
	}
	best := 0
	for i := 1; i < 3; i++ {
		if leadCount[i] > leadCount[best] {
			best = i
		}
	}
	c := int32(len(bfs))
	return nationChar(best), sumA / c, sumB / c, sumC / c
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
