package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// UNIDENTIFIED WEAPON events (WORLD_NEWS rows 47-52 and 81-83).
//
// A nation deploys a superweapon at ONE FIXED BATTLEFIELD, bans its own mercenaries from that battlefield
// while it stands, and the ban lifts when the weapon is destroyed or withdrawn.
//
// THE LOCATION IS NOT A PARAMETER. Unlike most WORLD_NEWS stories these rows carry no |placeholder| tokens
// at all -- the battlefield name is baked into the FMG text of each row. Row 47 literally says "sighted in
// Wakool". So the site is fixed per nation and cannot be chosen; picking the row IS picking the location:
//
//	Tarakia 'A' -> Wakool               (area 11, map 4)  appear 47  destroyed 50  withdrawn 81
//	Morskoj 'B' -> East Salma Woods     (area 17, map 4)  appear 48  destroyed 51  withdrawn 82
//	Sal Kar 'C' -> South Cemo Oil Field (area 18, map 4)  appear 49  destroyed 52  withdrawn 83
//
// SCOPE. This emits the NEWS and records the DEPLOYMENT (Battlefield.WeaponNation), and that deployment
// now DRIVES THE CLIENT: buildWorld raises the World (195) per-nation deploy byte for any nation with a
// weapon deployed here, which is what makes the weapon show on the war map. See applyWeaponDeploy.
//
// THE SELECTOR (found 2026-07-21, proven live). The client's weapon-state object is populated from the
// World (195) reply by the 0x6004 handler sub_823C1B10, which copies the per-nation DEPLOY byte from World
// body offset 436+n (= Tail[228+n]) to weaponObj+68/72/76. WorldMapScene_UpdateWeapons reads that field
// via GetWeaponEnableForNation and, when non-zero, calls SetBattlefieldWeaponFlag to light the weapon. So
// the selector is one wire byte per nation -- server-controlled, no title patch, no debug toggle. (The
// separate 28-byte record at Tail[28n..] -> weaponObj+260 is the weapon's durability/HP display state; the
// DB WeaponRecordHex field feeds it.)
//
// マップロックフラグ was tried first and REJECTED by testing (2026-07-21): it does not switch the area-info
// preview to the _01 variant, it just closes the battlefield to every nation including the attackers,
// making the weapon unkillable. WeaponNation therefore does not feed it -- it feeds the deploy byte above.
//
// WHAT THE MISSION ACTUALLY IS (Event/lua/JIT/event_201..203.lua, retail): a host-authoritative boss
// encounter with a 600s countdown, one per nation --
//
//	201 Tarakia  boss actor c0511_000  (Remo 0020)
//	202 Morskoj  boss actor c0521_000  (Remo 0030)
//	203 Sal Kar  boss actor c0531_000  (Remo 0040)
//
// Those scripts run at mission LAUNCH; the deploy byte above is what puts the battlefield into the state
// the war map shows.

// UnidentifiedWeaponPhase is one stage of a weapon's lifecycle.
type UnidentifiedWeaponPhase int

const (
	WeaponAppears UnidentifiedWeaponPhase = iota
	WeaponDestroyed
	WeaponWithdrawn
)

// ParseWeaponPhase maps a CLI phase name to its constant.
func ParseWeaponPhase(s string) (UnidentifiedWeaponPhase, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "appear", "appears":
		return WeaponAppears, true
	case "destroy", "destroyed":
		return WeaponDestroyed, true
	case "withdraw", "withdrawn":
		return WeaponWithdrawn, true
	}
	return 0, false
}

func (p UnidentifiedWeaponPhase) String() string {
	switch p {
	case WeaponAppears:
		return "appears"
	case WeaponDestroyed:
		return "destroyed"
	case WeaponWithdrawn:
		return "withdrawn"
	}
	return "unknown"
}

// UnidentifiedWeaponSite is the fixed deployment site for one nation's weapon. AreaID/MapID are the
// battlefield the story names -- the news text needs no ids, but they identify the map whose _01 variant
// the state is supposed to select, and the battlefield a future ban would gate.
type UnidentifiedWeaponSite struct {
	Nation      byte
	NationName  string
	Battlefield string
	AreaID      int32
	MapID       int32
	Rows        map[UnidentifiedWeaponPhase]int32
}

var unidentifiedWeaponSites = map[byte]UnidentifiedWeaponSite{
	'A': {
		Nation: 'A', NationName: "Tarakia", Battlefield: "Wakool", AreaID: 11, MapID: 4,
		Rows: map[UnidentifiedWeaponPhase]int32{WeaponAppears: 47, WeaponDestroyed: 50, WeaponWithdrawn: 81},
	},
	'B': {
		Nation: 'B', NationName: "Morskoj", Battlefield: "East Salma Woods", AreaID: 17, MapID: 4,
		Rows: map[UnidentifiedWeaponPhase]int32{WeaponAppears: 48, WeaponDestroyed: 51, WeaponWithdrawn: 82},
	},
	'C': {
		Nation: 'C', NationName: "Sal Kar", Battlefield: "South Cemo Oil Field", AreaID: 18, MapID: 4,
		Rows: map[UnidentifiedWeaponPhase]int32{WeaponAppears: 49, WeaponDestroyed: 52, WeaponWithdrawn: 83},
	},
}

// UnidentifiedWeaponSiteFor returns the fixed site for a nation code ('A'/'B'/'C'), case-insensitive.
func UnidentifiedWeaponSiteFor(nation byte) (UnidentifiedWeaponSite, bool) {
	if nation >= 'a' && nation <= 'z' {
		nation -= 'a' - 'A'
	}
	site, ok := unidentifiedWeaponSites[nation]
	return site, ok
}

// WeaponPhaseRow returns the WORLD_NEWS param row for a nation + phase.
func WeaponPhaseRow(nation byte, phase UnidentifiedWeaponPhase) (int32, bool) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return 0, false
	}
	row, ok := site.Rows[phase]
	return row, ok
}

// WeaponPhaseState is what CurrentWeaponPhase found in the event feed.
type WeaponPhaseState struct {
	Active bool                    // a weapon is currently deployed for this nation
	Last   UnidentifiedWeaponPhase // the most recent phase seen
	Found  bool                    // any weapon event at all was found for this nation
}

// CurrentWeaponPhase reports whether a nation currently has a weapon deployed, by scanning the event feed
// newest-first for that nation's three rows and taking the first hit. Used to reject nonsense transitions
// (destroying a weapon that was never deployed) before they reach the news feed, where they would read as
// a genuine story with no setup.
//
// It scans a bounded window rather than the whole collection: a weapon deployment that has scrolled past
// `limit` stories is effectively historical anyway, and the caller can override the guard.
func (r *WorldRepository) CurrentWeaponPhase(ctx context.Context, nation byte, limit int) (WeaponPhaseState, error) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return WeaponPhaseState{}, fmt.Errorf("unknown nation %q", string(nation))
	}
	byRow := make(map[int32]UnidentifiedWeaponPhase, len(site.Rows))
	for phase, row := range site.Rows {
		byRow[row] = phase
	}

	events, err := r.RecentEvents(ctx, limit)
	if err != nil {
		return WeaponPhaseState{}, err
	}
	for _, ev := range events {
		phase, hit := byRow[ev.TemplateID]
		if !hit {
			continue
		}
		return WeaponPhaseState{Active: phase == WeaponAppears, Last: phase, Found: true}, nil
	}
	return WeaponPhaseState{}, nil
}

// RecordUnidentifiedWeaponEvent writes the WORLD_NEWS event for a nation + phase. Slot1/Slot2 are left
// empty on purpose: these rows have no placeholder tokens, so nothing is substituted into the text.
func (r *WorldRepository) RecordUnidentifiedWeaponEvent(ctx context.Context, nation byte, phase UnidentifiedWeaponPhase, when time.Time) (int32, error) {
	row, ok := WeaponPhaseRow(nation, phase)
	if !ok {
		return 0, fmt.Errorf("no unidentified-weapon row for nation %q phase %s", string(nation), phase)
	}
	if when.IsZero() {
		when = time.Now()
	}
	return row, r.RecordEvent(ctx, EventRecord{CreatedAt: when.Unix(), TemplateID: row})
}

// WeaponBattleAreaID is the sentinel area id a weapon-destruction ("boss") mission reports itself under.
// Confirmed 2026-07-21 from a live capture: a South Cemo weapon mission arrived as a 1214 battle report with
// AreaID=99, MapID=4, the weapon as the LOSER (synthetic all-9s opponent team) and the attacking nation as
// the winner. It is a real battlefield in no area, so applyWeaponReport handles area 99 specially rather
// than trying to move occupation. Which nation's weapon it is comes from the loser nation, not the area.
const WeaponBattleAreaID = 99

// DURABILITY = OCCUPATION (proven in-game 2026-07-21). The weapon's durability bar is not a wire field of
// its own -- the client renders it from the deployed battlefield's occupation total, exactly like capture
// levels (which is why it "resets to full" when the served occupation is re-read on a map return). So we
// model durability as the weapon site's occupation: a freshly deployed weapon sits at the nation's full
// control (full bar), each successful attack drains that occupation, and destruction restores the site to
// the nation's 100% default (bar irrelevant -- the weapon is gone and normal missions resume).

// SetWeaponDeployed deploys a nation's weapon on its fixed battlefield at FULL durability -- it sets the site
// to that nation's 100% control (full occupation = full bar) and arms it to auto-destroy after
// missionsToDestroy successful attacks (0 = no auto-destroy; remove via the tool). Resets the hit counter.
//
// buildWorld reads the deployment (DeployedWeaponNations) and raises the World (195) per-nation deploy byte,
// which lights the weapon on the war map. It does NOT set Battlefield.Locked and does not feed マップロック
// フラグ -- that would close the battlefield to the attackers and make the weapon unkillable.
func (r *WorldRepository) SetWeaponDeployed(ctx context.Context, nation byte, missionsToDestroy int32) (UnidentifiedWeaponSite, error) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return site, fmt.Errorf("unknown nation %q", string(nation))
	}
	bf, err := r.BattlefieldByAreaMap(ctx, byte(site.AreaID), byte(site.MapID))
	if err != nil {
		return site, err
	}
	if bf == nil {
		return site, fmt.Errorf("battlefield %d/%d (%s) not found in the database", site.AreaID, site.MapID, site.Battlefield)
	}
	// Full durability = the weapon nation at 100% of the site (occupation = capacity), others zero.
	set := bson.M{
		"weaponNation": string(site.Nation), "weaponHits": int32(0),
		"occA": int32(0), "occB": int32(0), "occC": int32(0), occField(site.Nation): bf.Capacity,
	}
	update := bson.M{"$set": set}
	if missionsToDestroy > 0 {
		set["weaponMissionsToDestroy"] = missionsToDestroy
	} else {
		update["$unset"] = bson.M{"weaponMissionsToDestroy": ""}
	}
	_, err = r.battlefields.UpdateOne(ctx, bson.M{"areaId": site.AreaID, "mapId": site.MapID}, update)
	return site, err
}

// ClearWeaponDeployed removes a nation's weapon and restores the site to that nation's default 100% control
// (so normal missions resume) -- the same reset applied on auto-destruction. A no-op on occupation if no
// weapon was deployed there.
func (r *WorldRepository) ClearWeaponDeployed(ctx context.Context, nation byte) (UnidentifiedWeaponSite, error) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return site, fmt.Errorf("unknown nation %q", string(nation))
	}
	bf, err := r.BattlefieldByAreaMap(ctx, byte(site.AreaID), byte(site.MapID))
	if err != nil {
		return site, err
	}
	if bf == nil {
		return site, fmt.Errorf("battlefield %d/%d (%s) not found in the database", site.AreaID, site.MapID, site.Battlefield)
	}
	update := bson.M{"$unset": bson.M{"weaponNation": "", "weaponMissionsToDestroy": "", "weaponHits": ""}}
	if bf.WeaponNation != "" {
		update["$set"] = bson.M{"occA": int32(0), "occB": int32(0), "occC": int32(0), occField(bf.WeaponNation[0]): bf.Capacity}
	}
	_, err = r.battlefields.UpdateOne(ctx, bson.M{"areaId": site.AreaID, "mapId": site.MapID}, update)
	return site, err
}

// ApplyWeaponHit registers one successful destruction mission against the nation's deployed weapon and drives
// the durability model in a single step:
//   - not deployed for that nation -> found=false (a stray area-99 report the caller ignores);
//   - with a mission threshold, it drains the site's occupation to capacity*(N-hits)/N so the durability bar
//     steps down evenly, and on the Nth hit destroys the weapon: the deployment clears and the site snaps
//     back to the nation's 100% default (destroyed=true) so normal missions resume there;
//   - with no threshold (0), it only counts the hit (the weapon persists; remove it with the tool).
func (r *WorldRepository) ApplyWeaponHit(ctx context.Context, nation byte) (hits, threshold int32, destroyed, found bool, err error) {
	var bf Battlefield
	e := r.battlefields.FindOne(ctx, bson.M{"weaponNation": string(nation)}).Decode(&bf)
	if errors.Is(e, mongo.ErrNoDocuments) {
		return 0, 0, false, false, nil
	}
	if e != nil {
		return 0, 0, false, false, e
	}
	found = true
	hits = bf.WeaponHits + 1
	threshold = bf.WeaponMissionsToDestroy
	field := occField(nation)
	if field == "" {
		return hits, threshold, false, true, fmt.Errorf("unknown nation %q", string(nation))
	}

	if threshold > 0 && hits >= threshold {
		// Destroyed: clear the weapon and restore the site to the nation's 100% default control.
		destroyed = true
		_, err = r.battlefields.UpdateOne(ctx,
			bson.M{"areaId": bf.AreaID, "mapId": bf.MapID},
			bson.M{
				"$set":   bson.M{"occA": int32(0), "occB": int32(0), "occC": int32(0), field: bf.Capacity},
				"$unset": bson.M{"weaponNation": "", "weaponMissionsToDestroy": "", "weaponHits": ""},
			})
		return hits, threshold, destroyed, found, err
	}

	set := bson.M{"weaponHits": hits}
	if threshold > 0 {
		// Durability bar = occupation: drain proportionally toward zero over the N missions.
		set[field] = int32(int64(bf.Capacity) * int64(threshold-hits) / int64(threshold))
	}
	_, err = r.battlefields.UpdateOne(ctx, bson.M{"areaId": bf.AreaID, "mapId": bf.MapID}, bson.M{"$set": set})
	return hits, threshold, false, found, err
}

// DeployedWeaponNations returns the set of nations ('A'/'B'/'C') that currently have an unidentified
// weapon deployed on their fixed battlefield (any Battlefield.WeaponNation set to them). buildWorld uses
// it to raise the World (195) per-nation deploy byte, which is what actually makes the weapon appear on
// the war map -- see applyWeaponDeploy.
func (r *WorldRepository) DeployedWeaponNations(ctx context.Context) (map[byte]bool, error) {
	cur, err := r.battlefields.Find(ctx,
		bson.M{"weaponNation": bson.M{"$nin": bson.A{"", nil}}},
		options.Find().SetProjection(bson.M{"weaponNation": 1}))
	if err != nil {
		return nil, err
	}
	var bfs []Battlefield
	if err := cur.All(ctx, &bfs); err != nil {
		return nil, err
	}
	out := make(map[byte]bool, 3)
	for _, b := range bfs {
		if b.WeaponNation != "" {
			out[b.WeaponNation[0]] = true
		}
	}
	return out, nil
}

// WeaponDeployedNation returns the nation whose weapon is deployed on a battlefield, or 0 for none.
func (r *WorldRepository) WeaponDeployedNation(ctx context.Context, areaID, mapID int32) (byte, error) {
	var b Battlefield
	err := r.battlefields.FindOne(ctx, bson.M{"areaId": areaID, "mapId": mapID}).Decode(&b)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	if b.WeaponNation == "" {
		return 0, nil
	}
	return b.WeaponNation[0], nil
}

// ---------------------------------------------------------------------------
// Canonical unidentified-weapon world state (simulation support)
// ---------------------------------------------------------------------------
//
// OPERATOR HYPOTHESIS (2026-07-21), not yet confirmed: retail deployed a nation's superweapon when that
// nation had been beaten down to <= 10 occupation points ("orange dots") across the whole map AND had just
// retaken its weapon area from an occupier.
//
// The threshold is structurally suggestive rather than arbitrary: every area distributes exactly
// areaDotTotal = 5 dots (reset.areaDots), so <= 10 points is precisely "reduced to two areas" -- for
// Sal Kar, its HQ (area 3, Qara) plus its weapon area (18). That is a last-stand condition, which fits a
// desperation superweapon.
//
// WeaponLastStand builds that world state so it can be tested. It is a SIMULATION SETUP, not gameplay
// logic: nothing here decides when a weapon appears.

// WeaponHQArea returns a nation's capital area (1 Xeres / 2 Ostrov / 3 Qara).
func WeaponHQArea(nation byte) (int32, bool) {
	switch nation {
	case 'A':
		return 1, true
	case 'B':
		return 2, true
	case 'C':
		return 3, true
	}
	return 0, false
}

// LastStandPlan is the set of battlefield mutations WeaponLastStand would apply.
type LastStandPlan struct {
	Nation     byte
	HQArea     int32
	WeaponArea int32
	WeaponMap  int32
	Conqueror  byte
	Held       []string // battlefields handed to Nation
	Ceded      []string // battlefields handed away
	Unlocked   []string // capture-locks cleared
	DotsAfter  int32    // Nation's occupation points once applied
	DotsOtherA int32
	DotsOtherB int32
}

// WeaponLastStand drives a nation down to holding ONLY its HQ area and its weapon area, ceding every other
// battlefield to conqueror (or, for battlefields the conqueror cannot plausibly hold, to the remaining
// nation). Capture locks on the weapon area are cleared, because a locked battlefield accepts no missions
// and would make the area untestable.
//
// apply=false plans without writing.
func (r *WorldRepository) WeaponLastStand(ctx context.Context, nation, conqueror byte, apply bool) (LastStandPlan, error) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return LastStandPlan{}, fmt.Errorf("unknown nation %q", string(nation))
	}
	hq, _ := WeaponHQArea(site.Nation)
	if conqueror == site.Nation || occFieldName(conqueror) == "" {
		return LastStandPlan{}, fmt.Errorf("conqueror %q must be a different, valid nation", string(conqueror))
	}
	plan := LastStandPlan{
		Nation: site.Nation, HQArea: hq, WeaponArea: site.AreaID, WeaponMap: site.MapID, Conqueror: conqueror,
	}

	var all []Battlefield
	cur, err := r.battlefields.Find(ctx, bson.M{})
	if err != nil {
		return plan, err
	}
	if err := cur.All(ctx, &all); err != nil {
		return plan, err
	}

	for _, bf := range all {
		keep := bf.AreaID == hq || bf.AreaID == site.AreaID
		owner := conqueror
		if keep {
			owner = site.Nation
		}

		label := fmt.Sprintf("%d/%d", bf.AreaID, bf.MapID)
		if keep {
			plan.Held = append(plan.Held, label)
			plan.DotsAfter += bf.StrategicValue
		} else {
			plan.Ceded = append(plan.Ceded, label)
			if conqueror == 'A' {
				plan.DotsOtherA += bf.StrategicValue
			} else {
				plan.DotsOtherB += bf.StrategicValue
			}
		}

		// A capture lock on the weapon area would block the very missions this setup exists to allow.
		clearLock := bf.AreaID == site.AreaID && (bf.Locked || bf.DefeatedNation != "")
		if clearLock {
			plan.Unlocked = append(plan.Unlocked, label)
		}
		if !apply {
			continue
		}

		set := bson.M{"occA": int32(0), "occB": int32(0), "occC": int32(0)}
		set[occFieldName(owner)] = bf.Capacity // 100% to the owner
		update := bson.M{"$set": set}
		if clearLock {
			update["$unset"] = bson.M{"locked": "", "defeatedNation": "", "unlockAtBattle": ""}
		}
		if _, err := r.battlefields.UpdateOne(ctx,
			bson.M{"areaId": bf.AreaID, "mapId": bf.MapID}, update); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// occFieldName maps a nation char to its stored occupation field.
func occFieldName(n byte) string {
	switch n {
	case 'A':
		return "occA"
	case 'B':
		return "occB"
	case 'C':
		return "occC"
	}
	return ""
}

// RecordRegionRecapture publishes the "<loser> Abandons <area>" WORLD_NEWS story, i.e. the moment an area
// changes hands. Used by the weapon simulation to stage the "just retaken its weapon area" precondition;
// the normal path for this is RecordRegionCaptureEvent off a real battle report.
func (r *WorldRepository) RecordRegionRecapture(ctx context.Context, areaID int32, loser byte, when time.Time) error {
	row, ok := regionCaptureRow(loser)
	if !ok {
		return fmt.Errorf("no region-capture row for losing nation %q", string(loser))
	}
	if when.IsZero() {
		when = time.Now()
	}
	return r.RecordEvent(ctx, EventRecord{
		CreatedAt:  when.Unix(),
		TemplateID: row,
		Slot1:      areaNameSlot(areaID),
	})
}
