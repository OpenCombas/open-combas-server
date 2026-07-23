package predict

import "ChromehoundsStatusServer/reset"

// unlockThreshold mirrors server.UnlockBattleThreshold: battles a defeated nation must fight before a
// battlefield it lost reopens.
const unlockThreshold = 10

// bf is one battlefield's mutable state. occ is [A,B,C]. Mirrors server.Battlefield (only the fields the
// war kernel touches).
type bf struct {
	area, mapID  int32
	capacity, sv int32
	occ          [3]int32
	locked       bool
	defeated     byte  // nation locked out
	unlockAt     int32 // defeated nation's battleCount at which it reopens
}

func idxOf(n byte) int {
	switch n {
	case natA:
		return 0
	case natB:
		return 1
	case natC:
		return 2
	}
	return -1
}
func charOf(i int) byte { return nations[i] }

// leadFaction: argmax of (a,b,c) with A as default and strict '>' (so ties keep A>B>C) -- identical to the
// server. level 0 means no occupation at all.
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

// lead returns the battlefield's current holder ('A'/'B'/'C'), or 0 if unoccupied.
func (b *bf) lead() byte {
	i, lvl := leadFaction(b.occ[0], b.occ[1], b.occ[2])
	if lvl == 0 {
		return 0
	}
	return charOf(i)
}

// leadAfterDelta: holder AFTER winner +delta (cap) / loser -delta (floor), without mutating.
func (b *bf) leadAfterDelta(winner, loser byte, delta int32) byte {
	occ := b.occ
	if wi := idxOf(winner); wi >= 0 {
		if occ[wi] += delta; occ[wi] > b.capacity {
			occ[wi] = b.capacity
		}
	}
	if li := idxOf(loser); li >= 0 {
		if occ[li] -= delta; occ[li] < 0 {
			occ[li] = 0
		}
	}
	if i, lvl := leadFaction(occ[0], occ[1], occ[2]); lvl > 0 {
		return charOf(i)
	}
	return 0
}

// World is the in-memory war state.
type World struct {
	bfs     map[[2]int32]*bf
	byArea  map[int32][]*bf
	areas   []int32
	adj     map[int32][]int32
	count   map[byte]int32 // per-nation battle counter (unlock clock)
	hqLost  map[byte]byte  // nation -> captor, if dissolved (revival lockout)
	capDist map[byte]map[int32]int // enemy-capital -> hop distances (for push bias), keyed by capital nation
	// ownerCache memoises areaOwner (depends only on the area's battlefield occupation, which changes only
	// via this area's own writes -> invalidate that one area on each write). Hot path at high battle rates.
	ownerCache map[int32]byte
}

func (w *World) dirtyArea(area int32) { delete(w.ownerCache, area) }

// NewWorld builds the world from the canonical reset seed at the given downscale.
func NewWorld(downscale int32) *World {
	w := &World{
		bfs: map[[2]int32]*bf{}, byArea: map[int32][]*bf{},
		adj: buildAdjacency(), count: map[byte]int32{}, hqLost: map[byte]byte{},
		ownerCache: map[int32]byte{},
	}
	for _, s := range reset.SeededBattlefields(downscale) {
		b := &bf{area: s.AreaID, mapID: s.MapID, capacity: s.Capacity, sv: s.StrategicValue}
		b.occ[idxOf(s.Owner)] = s.Capacity
		w.bfs[[2]int32{s.AreaID, s.MapID}] = b
		w.byArea[s.AreaID] = append(w.byArea[s.AreaID], b)
	}
	seen := map[int32]bool{}
	for a := range w.byArea {
		if !seen[a] {
			seen[a] = true
			w.areas = append(w.areas, a)
		}
	}
	w.capDist = map[byte]map[int32]int{}
	for _, n := range nations {
		w.capDist[n] = hopDist(w.adj, capitalArea(n))
	}
	return w
}

// areaOwner ports server.areaSummaryFrom: owner = nation with most strategic-value among battlefields it
// leads, ties broken by total occupation; empty area -> 'A'.
func (w *World) areaOwner(area int32) byte {
	if o, ok := w.ownerCache[area]; ok {
		return o
	}
	bfs := w.byArea[area]
	if len(bfs) == 0 {
		return natA
	}
	var points, occTotal [3]int32
	for _, b := range bfs {
		occTotal[0] += b.occ[0]
		occTotal[1] += b.occ[1]
		occTotal[2] += b.occ[2]
		if i, lvl := leadFaction(b.occ[0], b.occ[1], b.occ[2]); lvl > 0 {
			points[i] += b.sv
		}
	}
	best := 0
	for i := 1; i < 3; i++ {
		if points[i] > points[best] || (points[i] == points[best] && occTotal[i] > occTotal[best]) {
			best = i
		}
	}
	o := charOf(best)
	w.ownerCache[area] = o
	return o
}

func (w *World) areasOwnedBy(n byte) []int32 {
	var out []int32
	for _, a := range w.areas {
		if w.areaOwner(a) == n {
			out = append(out, a)
		}
	}
	return out
}

// captureSet snaps a battlefield 100% to winner and locks the defeated out until unlockAt.
func (b *bf) captureSet(winner, defeated byte, unlockAt int32) {
	b.occ = [3]int32{}
	b.occ[idxOf(winner)] = b.capacity
	b.locked, b.defeated, b.unlockAt = true, defeated, unlockAt
}

// battle is one applied mission outcome (already resolved winner/loser).
type battle struct {
	area, mapID  int32
	winner, loser byte
	delta        int32
}

// Apply ports server.applyBattle: accumulate occupation, flip+lock on crossover, surrender the area if the
// owner changes, and handle HQ fall (dissolution+cascade+lockout) / revival. Locked battlefields are inert.
func (w *World) Apply(m battle) {
	b := w.bfs[[2]int32{m.area, m.mapID}]
	if b == nil || b.locked {
		return
	}
	beforeLead := b.lead()
	newLead := b.leadAfterDelta(m.winner, m.loser, m.delta)
	changedHands := beforeLead != 0 && newLead != 0 && newLead != beforeLead
	beforeOwner := w.areaOwner(m.area)

	w.count[m.winner]++
	w.count[m.loser]++
	winnerCount, loserCount := w.count[m.winner], w.count[m.loser]

	if changedHands {
		defeated := beforeLead
		defeatedCount := loserCount
		if defeated != m.loser {
			defeatedCount = w.count[defeated]
		}
		b.captureSet(m.winner, defeated, defeatedCount+unlockThreshold)
	} else {
		// accumulate: winner +delta (cap), loser -delta (floor)
		if wi := idxOf(m.winner); wi >= 0 {
			if b.occ[wi] += m.delta; b.occ[wi] > b.capacity {
				b.occ[wi] = b.capacity
			}
		}
		if li := idxOf(m.loser); li >= 0 {
			if b.occ[li] -= m.delta; b.occ[li] < 0 {
				b.occ[li] = 0
			}
		}
	}
	w.dirtyArea(m.area) // occupation changed -> this area's owner may have

	w.unlockExpired(m.winner, winnerCount)
	w.unlockExpired(m.loser, loserCount)

	if !changedHands {
		return
	}

	afterOwner := w.areaOwner(m.area)
	areaFlipped := beforeOwner != 0 && afterOwner != 0 && beforeOwner != afterOwner
	if !areaFlipped {
		return
	}

	if hq := capitalOf(m.area); hq != 0 {
		switch {
		case beforeOwner == hq:
			w.hqFall(m.area, hq, afterOwner)
			return
		case afterOwner == hq:
			if w.hqLost[hq] != 0 {
				w.flipAreaUnlocked(m.area, hq)
				delete(w.hqLost, hq)
				return
			}
		}
	}
	// ordinary area surrender: whole area flips + locks the former owner out
	w.flipAndLockArea(m.area, afterOwner, beforeOwner, w.count[beforeOwner]+unlockThreshold)
}

func (w *World) unlockExpired(n byte, count int32) {
	for _, b := range w.bfs {
		if b.locked && b.defeated == n && b.unlockAt <= count {
			b.locked, b.defeated, b.unlockAt = false, 0, 0
		}
	}
}

func (w *World) flipAndLockArea(area int32, winner, defeated byte, unlockAt int32) {
	for _, b := range w.byArea[area] {
		b.captureSet(winner, defeated, unlockAt)
	}
	w.dirtyArea(area)
}

func (w *World) flipAreaUnlocked(area int32, winner byte) {
	for _, b := range w.byArea[area] {
		b.occ = [3]int32{}
		b.occ[idxOf(winner)] = b.capacity
		b.locked, b.defeated, b.unlockAt = false, 0, 0
	}
	w.dirtyArea(area)
}

// hqFall: fallen capital + every other area the loser still holds cascade to captor (unlocked); loser enters
// revival lockout.
func (w *World) hqFall(area int32, loser, captor byte) {
	w.flipAreaUnlocked(area, captor)
	for _, a := range w.areasOwnedBy(loser) {
		if a != area {
			w.flipAreaUnlocked(a, captor)
		}
	}
	w.hqLost[loser] = captor
}

// --- status helpers for the simulation loop ---

func (w *World) dissolved(n byte) bool { return w.hqLost[n] != 0 }

// capitalsHeld returns how many of the three capital areas nation n currently owns.
func (w *World) capitalsHeld(n byte) int {
	c := 0
	for _, cap := range []int32{1, 2, 3} {
		if w.areaOwner(cap) == n {
			c++
		}
	}
	return c
}

// victor returns the nation that holds all three capitals, or 0 if none does yet.
func (w *World) victor() byte {
	for _, n := range nations {
		if w.capitalsHeld(n) == 3 {
			return n
		}
	}
	return 0
}

// anyHQFallen reports whether any capital is held by a nation other than its default (the first-dissolution
// milestone).
func (w *World) anyHQFallen() bool {
	for _, cap := range []int32{1, 2, 3} {
		if w.areaOwner(cap) != capitalOf(cap) {
			return true
		}
	}
	return false
}
