package predict

import (
	"math"
	"math/rand"
)

// baseCPUWinHist is the UN-downscaled CPU-win occupation histogram (the pre-2026-07-17 regime, ~20 occ per
// CPU win). The live "CPU downscale" multiplies these: factor 0.1 -> ~2 occ (the current regime, 07-17+),
// factor 1.0 -> ~20 (no downscale). The CPU-LOSS drain is deliberately NOT scaled by this (operator design:
// losing to a CPU stays a large, fixed setback so CPU play is discouraged) -- see cpuLoss below.
var baseCPUWinHist = map[int32]int{10: 2, 15: 1, 16: 26, 19: 31, 20: 116}

// buildCPUWin scales the un-downscaled CPU-win histogram by factor (each occ value * factor, floored at 1).
func buildCPUWin(factor float64) *dist {
	scaled := map[int32]int{}
	for v, c := range baseCPUWinHist {
		nv := int32(math.Round(float64(v) * factor))
		if nv < 1 {
			nv = 1
		}
		scaled[nv] += c
	}
	return newDist(scaled)
}

// dist is a weighted sampler over integer occupation deltas (from an empirical histogram).
type dist struct {
	vals []int32
	cum  []float64
	tot  float64
}

func newDist(hist map[int32]int) *dist {
	d := &dist{}
	// stable order for reproducibility
	keys := make([]int32, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		d.tot += float64(hist[k])
		d.vals = append(d.vals, k)
		d.cum = append(d.cum, d.tot)
	}
	return d
}

func (d *dist) sample(r *rand.Rand) int32 {
	x := r.Float64() * d.tot
	for i, c := range d.cum {
		if x < c {
			return d.vals[i]
		}
	}
	return d.vals[len(d.vals)-1]
}

// Params holds the calibrated battle-generation model. Defaults come from 30 days of PRODUCTION combas logs
// (monitoring Loki, container opencombas-combas-server-1) as measured on 2026-07-23.
type Params struct {
	RatePerDay float64                    // battles/day (Poisson mean per day)
	PvPFrac    float64                    // fraction of battles that are PvP (vs CPU)
	CPUWinRate float64                    // P(the real attacker beats the CPU defender)
	WinProb    map[byte]map[byte]float64  // WinProb[X][Y] = P(X beats Y) in PvP (head-to-head)
	Activity     map[byte]float64          // relative attack initiation weight per nation
	PushBias     float64                   // [0,1]: prob a nation targets the frontier area CLOSEST to an enemy capital (advance) vs a random frontier area (churn)
	CPUDownscale float64                   // multiplies the CPU-WIN occupation gain (0.1 = current regime, 1.0 = un-downscaled ~20)
	pvpDelta     *dist
	cpuWin       *dist
	cpuLoss      *dist
}

// WithCPUDownscale returns a copy of p with the CPU-win downscale set to factor (rebuilding the cpuWin
// sampler). The CPU-loss drain is unchanged.
func (p Params) WithCPUDownscale(factor float64) Params {
	p.CPUDownscale = factor
	p.cpuWin = buildCPUWin(factor)
	return p
}

// WithStrengthScale returns a copy of p with the nation-strength IMBALANCE scaled by `scale`, relative to a
// balanced baseline. It pulls each PvP win-probability toward 0.5 and each activity weight toward the mean:
//
//	scale 0   -> perfect parity (every PvP is a coin flip, equal activity) => a CONTESTED, non-deterministic war
//	scale 1   -> the measured imbalance, unchanged (the default)
//	scale >1  -> the measured imbalance AMPLIFIED (the strong nation runs away even harder)
//
// Antisymmetry is preserved (P(X>Y)+P(Y>X)=1), and both maps are clamped to sane ranges. Apply this AFTER
// loading a calibration (it scales whatever the current strengths are), and re-derive it from `base` each
// sweep cell so the scaling never compounds.
func (p Params) WithStrengthScale(scale float64) Params {
	wp := map[byte]map[byte]float64{}
	for x, row := range p.WinProb {
		m := map[byte]float64{}
		for y, v := range row {
			nv := 0.5 + scale*(v-0.5)
			if nv < 0 {
				nv = 0
			} else if nv > 1 {
				nv = 1
			}
			m[y] = nv
		}
		wp[x] = m
	}
	p.WinProb = wp

	var sum float64
	n := 0
	for _, a := range p.Activity {
		sum += a
		n++
	}
	if n > 0 {
		mean := sum / float64(n)
		act := map[byte]float64{}
		for k, a := range p.Activity {
			nv := mean + scale*(a-mean)
			if nv < 0.001 {
				nv = 0.001
			}
			act[k] = nv
		}
		p.Activity = act
	}
	return p
}

// DefaultParams returns the production-calibrated model. PushBias defaults to 0.5 (neither pure advance nor
// pure churn); it is the key behavioural knob and should be swept.
func DefaultParams() Params {
	wp := map[byte]map[byte]float64{
		natA: {natB: 0.80, natC: 0.405},
		natB: {natA: 0.20, natC: 0.143},
		natC: {natA: 0.595, natB: 0.857},
	}
	return Params{
		RatePerDay: 82.7,
		PvPFrac:    0.096,
		CPUWinRate: 0.962,
		WinProb:    wp,
		// PvP participation per nation (player-activity proxy from head-to-head counts): A 141, B 118, C 219.
		Activity: map[byte]float64{natA: 141, natB: 118, natC: 219},
		PushBias:     0.5,
		CPUDownscale: 0.1, // current regime (07-17+): CPU wins give ~2 occ
		pvpDelta:     newDist(map[int32]int{49: 2, 50: 5, 79: 5, 99: 152, 100: 75}),
		// CPU-win gain is the un-downscaled base * CPUDownscale (0.1 -> ~2, matching post-07-17 production).
		cpuWin: buildCPUWin(0.1),
		// CPU-LOSS drain (~19), post-07-17; NOT scaled by CPUDownscale (operator design -- see baseCPUWinHist).
		cpuLoss: newDist(map[int32]int{9: 4, 15: 6, 16: 2, 19: 46, 20: 18}),
	}
}

// frontier returns the battlefields nation n may currently attack, honouring adjacency, locks, and the
// revival lockout. A dissolved nation may only attack its own captured capital.
func (w *World) frontier(n byte) []*bf {
	if w.dissolved(n) {
		cap := capitalArea(n)
		var out []*bf
		for _, b := range w.byArea[cap] {
			if !b.locked && b.lead() != n {
				out = append(out, b)
			}
		}
		return out
	}
	owned := map[int32]bool{}
	for _, a := range w.areasOwnedBy(n) {
		owned[a] = true
	}
	targetArea := map[int32]bool{}
	for a := range owned {
		for _, nb := range w.adj[a] {
			if !owned[nb] && w.areaOwner(nb) != n {
				targetArea[nb] = true
			}
		}
	}
	var out []*bf
	for a := range targetArea {
		for _, b := range w.byArea[a] {
			if !b.locked && b.lead() != n {
				out = append(out, b)
			}
		}
	}
	return out
}

// enemyCapDist is n's distance from `area` to its NEAREST enemy capital (smaller = more "forward").
func (w *World) enemyCapDist(n byte, area int32) int {
	best := 1 << 30
	for _, e := range nations {
		if e == n {
			continue
		}
		if d, ok := w.capDist[e][area]; ok && d < best {
			best = d
		}
	}
	return best
}

// pickTarget chooses one battlefield from a frontier, applying PushBias at the AREA level.
func (w *World) pickTarget(n byte, front []*bf, p Params, r *rand.Rand) *bf {
	if len(front) == 0 {
		return nil
	}
	if r.Float64() < p.PushBias {
		// advance: restrict to the frontier area(s) closest to an enemy capital, then random bf there.
		bestDist := 1 << 30
		for _, b := range front {
			if d := w.enemyCapDist(n, b.area); d < bestDist {
				bestDist = d
			}
		}
		var fwd []*bf
		for _, b := range front {
			if w.enemyCapDist(n, b.area) == bestDist {
				fwd = append(fwd, b)
			}
		}
		return fwd[r.Intn(len(fwd))]
	}
	return front[r.Intn(len(front))]
}

// nextBattle samples one resolved battle from the current world state, or ok=false if nobody can attack.
func (w *World) nextBattle(p Params, r *rand.Rand) (battle, bool) {
	// pick an attacker among nations that have a frontier, weighted by activity.
	type cand struct {
		n     byte
		front []*bf
		wgt   float64
	}
	var cands []cand
	var total float64
	for _, n := range nations {
		f := w.frontier(n)
		if len(f) == 0 {
			continue
		}
		wgt := p.Activity[n]
		cands = append(cands, cand{n, f, wgt})
		total += wgt
	}
	if len(cands) == 0 {
		return battle{}, false
	}
	x := r.Float64() * total
	var att cand
	for _, c := range cands {
		if x < c.wgt {
			att = c
			break
		}
		x -= c.wgt
	}
	if att.n == 0 {
		att = cands[len(cands)-1]
	}
	tgt := w.pickTarget(att.n, att.front, p, r)
	if tgt == nil {
		return battle{}, false
	}
	defender := tgt.lead()

	if r.Float64() < p.PvPFrac {
		if r.Float64() < p.WinProb[att.n][defender] {
			return battle{tgt.area, tgt.mapID, att.n, defender, p.pvpDelta.sample(r)}, true
		}
		return battle{tgt.area, tgt.mapID, defender, att.n, p.pvpDelta.sample(r)}, true
	}
	// CPU: the real attacker usually wins (small gain); the rare loss is a large drain to the defender.
	if r.Float64() < p.CPUWinRate {
		return battle{tgt.area, tgt.mapID, att.n, defender, p.cpuWin.sample(r)}, true
	}
	return battle{tgt.area, tgt.mapID, defender, att.n, p.cpuLoss.sample(r)}, true
}
