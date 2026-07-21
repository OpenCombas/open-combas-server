package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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
// SCOPE. This emits the NEWS and records the DEPLOYMENT (Battlefield.WeaponNation). The deployment is
// currently BOOKKEEPING ONLY -- it changes nothing the client can see.
//
// マップロックフラグ was tried as the map-state lever and REJECTED by testing (2026-07-21): it does not
// switch the area-info preview to the _01 variant, it just closes the battlefield to every nation
// including the attackers, making the weapon unkillable. WeaponNation therefore does not feed it.
//
// WHAT THE MISSION ACTUALLY IS (Event/lua/JIT/event_201..203.lua, retail): a host-authoritative boss
// encounter with a 600s countdown, one per nation --
//
//	201 Tarakia  boss actor c0511_000  (Remo 0020)
//	202 Morskoj  boss actor c0521_000  (Remo 0030)
//	203 Sal Kar  boss actor c0531_000  (Remo 0040)
//
// Those scripts run at mission LAUNCH, so they show what the mission is, not how a battlefield is put into
// the state. The selector remains unfound: it is in neither the area (196), area-info (197) nor
// battlefield-detail (198) records, all of whose schemas are fully accounted for.

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

// SetWeaponDeployed records a nation's weapon as deployed on its fixed battlefield. Returns the site.
//
// This is BOOKKEEPING ONLY today: it raises no wire flag and changes nothing the client sees. It does not
// set Battlefield.Locked, and it deliberately no longer feeds マップロックフラグ -- doing so was tested
// and closed the battlefield to the attackers, making the weapon unkillable.
func (r *WorldRepository) SetWeaponDeployed(ctx context.Context, nation byte) (UnidentifiedWeaponSite, error) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return site, fmt.Errorf("unknown nation %q", string(nation))
	}
	res, err := r.battlefields.UpdateOne(ctx,
		bson.M{"areaId": site.AreaID, "mapId": site.MapID},
		bson.M{"$set": bson.M{"weaponNation": string(site.Nation)}},
	)
	if err != nil {
		return site, err
	}
	if res.MatchedCount == 0 {
		return site, fmt.Errorf("battlefield %d/%d (%s) not found in the database", site.AreaID, site.MapID, site.Battlefield)
	}
	return site, nil
}

// ClearWeaponDeployed removes a nation's weapon deployment record.
func (r *WorldRepository) ClearWeaponDeployed(ctx context.Context, nation byte) (UnidentifiedWeaponSite, error) {
	site, ok := UnidentifiedWeaponSiteFor(nation)
	if !ok {
		return site, fmt.Errorf("unknown nation %q", string(nation))
	}
	res, err := r.battlefields.UpdateOne(ctx,
		bson.M{"areaId": site.AreaID, "mapId": site.MapID},
		bson.M{"$unset": bson.M{"weaponNation": ""}},
	)
	if err != nil {
		return site, err
	}
	if res.MatchedCount == 0 {
		return site, fmt.Errorf("battlefield %d/%d (%s) not found in the database", site.AreaID, site.MapID, site.Battlefield)
	}
	return site, nil
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
