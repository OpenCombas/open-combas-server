package server

import (
	"context"
	"fmt"
	"strings"
	"time"
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
// SCOPE: this file only emits the NEWS. The mercenary ban the stories describe is a real gameplay effect
// (that battlefield should be closed to the owning nation's players while the weapon stands) and is NOT
// implemented here -- see UnidentifiedWeaponSite for the battlefield ids a future lock would need.

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
// battlefield the story names; they are informational here (the news text needs no ids) but are what a
// future mercenary-ban implementation would lock.
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
