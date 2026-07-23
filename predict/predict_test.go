package predict

import "testing"

func mkbf(area, mp, cap, sv int32, owner byte) *bf {
	b := &bf{area: area, mapID: mp, capacity: cap, sv: sv}
	b.occ[idxOf(owner)] = cap
	return b
}

func testWorld(bfs ...*bf) *World {
	w := &World{bfs: map[[2]int32]*bf{}, byArea: map[int32][]*bf{}, adj: buildAdjacency(), count: map[byte]int32{}, hqLost: map[byte]byte{}, ownerCache: map[int32]byte{}}
	seen := map[int32]bool{}
	for _, b := range bfs {
		w.bfs[[2]int32{b.area, b.mapID}] = b
		w.byArea[b.area] = append(w.byArea[b.area], b)
		if !seen[b.area] {
			seen[b.area] = true
			w.areas = append(w.areas, b.area)
		}
	}
	w.capDist = map[byte]map[int32]int{}
	for _, n := range nations {
		w.capDist[n] = hopDist(w.adj, capitalArea(n))
	}
	return w
}

// Crossover: a battlefield flips only once accumulated occupation makes the attacker overtake, then snaps
// 100% to the winner and locks the loser out; a locked battlefield is inert until it unlocks.
func TestCrossoverFlipAndLock(t *testing.T) {
	b := mkbf(50, 1, 100, 2, 'A')
	w := testWorld(b)

	w.Apply(battle{50, 1, 'B', 'A', 30}) // A70 B30 -> still A
	if b.lead() != 'A' || b.locked {
		t.Fatalf("after 1 push: lead=%c locked=%v, want A/false (occ %v)", b.lead(), b.locked, b.occ)
	}
	w.Apply(battle{50, 1, 'B', 'A', 30}) // A40 B60 -> crossover to B, snap+lock
	if b.lead() != 'B' || !b.locked || b.defeated != 'A' {
		t.Fatalf("after crossover: lead=%c locked=%v defeated=%c, want B/true/A", b.lead(), b.locked, b.defeated)
	}
	if b.occ != [3]int32{0, 100, 0} {
		t.Fatalf("winner-takes-all should snap to 100%%, got %v", b.occ)
	}
	// locked -> inert
	w.Apply(battle{50, 1, 'A', 'B', 100})
	if b.lead() != 'B' {
		t.Fatalf("locked battlefield must ignore reports, lead=%c", b.lead())
	}
	// reaching the unlock threshold reopens it
	w.unlockExpired('A', b.unlockAt)
	if b.locked {
		t.Fatalf("battlefield should reopen once A reaches unlockAt=%d", b.unlockAt)
	}
}

// A battlefield capture that tips the area's strategic-value majority surrenders the WHOLE area.
func TestAreaSurrender(t *testing.T) {
	b1 := mkbf(60, 1, 100, 3, 'A')
	b2 := mkbf(60, 2, 100, 1, 'A')
	b3 := mkbf(60, 3, 100, 1, 'A')
	w := testWorld(b1, b2, b3)

	w.Apply(battle{60, 1, 'B', 'A', 100}) // flip the sv-3 battlefield -> area owner A(2) -> B(3)
	if w.areaOwner(60) != 'B' {
		t.Fatalf("area owner = %c, want B", w.areaOwner(60))
	}
	for _, b := range []*bf{b1, b2, b3} {
		if b.lead() != 'B' || !b.locked {
			t.Fatalf("battlefield %d not surrendered: lead=%c locked=%v", b.mapID, b.lead(), b.locked)
		}
	}
}

// An HQ (capital) falling away from its default nation dissolves it: the capital + its other areas cascade to
// the captor and it enters revival lockout (may only attack its own capital).
func TestHQFallCascadeAndLockout(t *testing.T) {
	cap := mkbf(2, 1, 100, 1, 'B') // Ostrov, B's capital
	other := mkbf(8, 1, 100, 1, 'B') // another B-held area
	w := testWorld(cap, other)

	w.Apply(battle{2, 1, 'C', 'B', 100}) // C takes the capital
	if w.areaOwner(2) != 'C' {
		t.Fatalf("capital owner = %c, want C", w.areaOwner(2))
	}
	if !w.dissolved('B') || w.hqLost['B'] != 'C' {
		t.Fatalf("B should be dissolved to C, hqLost=%v", w.hqLost)
	}
	if w.areaOwner(8) != 'C' {
		t.Fatalf("cascade failed: area 8 owner = %c, want C", w.areaOwner(8))
	}
	if cap.locked {
		t.Fatalf("a fallen capital must stay UNLOCKED so the dissolved nation can retake it")
	}
	// revival lockout: B may only attack its own capital (area 2), nothing else.
	f := w.frontier('B')
	for _, b := range f {
		if b.area != 2 {
			t.Fatalf("dissolved B must be restricted to its capital, got a target in area %d", b.area)
		}
	}
}

// The canonical reset state starts with each capital held by its default nation and no victor.
func TestSeedInitialState(t *testing.T) {
	w := NewWorld(20)
	if w.areaOwner(1) != 'A' || w.areaOwner(2) != 'B' || w.areaOwner(3) != 'C' {
		t.Fatalf("capital owners = %c/%c/%c, want A/B/C", w.areaOwner(1), w.areaOwner(2), w.areaOwner(3))
	}
	if w.victor() != 0 || w.anyHQFallen() {
		t.Fatalf("fresh season should have no victor and no fallen HQ")
	}
	if len(w.areas) != 22 {
		t.Fatalf("expected 22 areas, got %d", len(w.areas))
	}
}

// The model must SPAN the observed reality: pure border-churn essentially never produces a conquest victor
// within a season (matching production: 0 HQ falls in 30 days), while a strong forward push under heavy
// downscale CAN reach one -- so the sim brackets stall vs breakthrough rather than always doing one.
func TestBehaviourSpansStallToVictor(t *testing.T) {
	p := DefaultParams()
	p.PushBias = 0 // churn the border
	churn := MonteCarlo(40, p, 120, 60, 1)
	if churn.VictorFrac() > 0.25 {
		t.Errorf("pure churn should rarely win in 60d, got %.0f%%", 100*churn.VictorFrac())
	}
	p.PushBias = 1 // always advance toward an enemy capital
	push := MonteCarlo(160, p, 120, 180, 1)
	if push.VictorFrac() == 0 {
		t.Errorf("full push + heavy downscale over 180d should sometimes reach a victor, got 0%%")
	}
}

// WithStrengthScale is identity at 1, perfect parity at 0, and turns the deterministic (C-locked) outcome
// into a contested one as it approaches parity.
func TestStrengthScale(t *testing.T) {
	p := DefaultParams()

	if id := p.WithStrengthScale(1); id.WinProb['C']['B'] != p.WinProb['C']['B'] || id.Activity['C'] != p.Activity['C'] {
		t.Errorf("scale 1 must be identity")
	}
	par := p.WithStrengthScale(0)
	for x, row := range par.WinProb {
		for y, v := range row {
			if v != 0.5 {
				t.Errorf("parity WinProb[%c][%c] = %v, want 0.5", x, y, v)
			}
		}
	}
	var a0 float64 = -1
	for _, a := range par.Activity {
		if a0 < 0 {
			a0 = a
		} else if a != a0 {
			t.Errorf("parity activity must be equal, got %v", par.Activity)
		}
	}

	// Behaviour: converging config (heavy downscale + eased CPU + push). At parity no single nation runs away;
	// at measured strength C is locked in.
	conf := func(pp Params) Params { pp = pp.WithCPUDownscale(0.5); pp.PushBias = 0.5; return pp }
	parity := MonteCarlo(80, conf(p.WithStrengthScale(0)), 150, 90, 1)
	maxShare := 0
	for _, n := range nations {
		if parity.Victors[n] > maxShare {
			maxShare = parity.Victors[n]
		}
	}
	if parity.VictorCount == 0 || float64(maxShare)/float64(parity.VictorCount) > 0.85 {
		t.Errorf("parity should be contested; top nation won %d/%d", maxShare, parity.VictorCount)
	}
	measured := MonteCarlo(80, conf(p.WithStrengthScale(1)), 150, 90, 1)
	if measured.VictorCount == 0 || float64(measured.Victors['C'])/float64(measured.VictorCount) < 0.85 {
		t.Errorf("measured strength should lock C in; C won %d/%d", measured.Victors['C'], measured.VictorCount)
	}
}
