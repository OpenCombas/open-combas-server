package predict

import (
	"math"
	"math/rand"
	"sort"
)

// TrialResult is the outcome of one simulated season.
type TrialResult struct {
	FirstHQDay int  // day the first capital fell, -1 if none within the cap
	VictorDay  int  // day a nation held all 3 capitals, -1 if none
	Victor     byte // that nation, 0 if none
	Battles    int
}

func poisson(lambda float64, r *rand.Rand) int {
	// battles/day is large, so a normal approximation is faithful and fast.
	n := int(math.Round(lambda + math.Sqrt(lambda)*r.NormFloat64()))
	if n < 0 {
		return 0
	}
	return n
}

// RunTrial simulates up to maxDays of war at the given downscale + params and reports when (if ever) the
// war reaches a first-capital-fall and a single-nation victor (all 3 capitals).
func RunTrial(downscale int32, p Params, maxDays int, r *rand.Rand) TrialResult {
	w := NewWorld(downscale)
	res := TrialResult{FirstHQDay: -1, VictorDay: -1}
	for day := 1; day <= maxDays; day++ {
		for i, nb := 0, poisson(p.RatePerDay, r); i < nb; i++ {
			b, ok := w.nextBattle(p, r)
			if !ok {
				break
			}
			w.Apply(b)
			res.Battles++
		}
		if res.FirstHQDay < 0 && w.anyHQFallen() {
			res.FirstHQDay = day
		}
		if v := w.victor(); v != 0 {
			res.VictorDay, res.Victor = day, v
			break
		}
	}
	return res
}

// Stats aggregates a Monte-Carlo batch for one (downscale, pushBias).
type Stats struct {
	Downscale                    int32
	PushBias                     float64
	Trials, MaxDays              int
	VictorCount, FirstHQCount    int
	VictorDays                   []int // only trials that reached a victor
	FirstHQDays                  []int
	Victors                      map[byte]int
}

func (s Stats) VictorFrac() float64  { return float64(s.VictorCount) / float64(s.Trials) }
func (s Stats) FirstHQFrac() float64 { return float64(s.FirstHQCount) / float64(s.Trials) }

func pct(xs []int, q float64) int {
	if len(xs) == 0 {
		return -1
	}
	cp := append([]int(nil), xs...)
	sort.Ints(cp)
	i := int(q * float64(len(cp)-1))
	return cp[i]
}
func (s Stats) MedVictorDay() int { return pct(s.VictorDays, 0.5) }
func (s Stats) P10VictorDay() int { return pct(s.VictorDays, 0.1) }
func (s Stats) P90VictorDay() int { return pct(s.VictorDays, 0.9) }
func (s Stats) MedFirstHQDay() int { return pct(s.FirstHQDays, 0.5) }

// MonteCarlo runs `trials` seasons for one (downscale, pushBias) and aggregates.
func MonteCarlo(downscale int32, p Params, trials, maxDays int, seed int64) Stats {
	r := rand.New(rand.NewSource(seed))
	s := Stats{Downscale: downscale, PushBias: p.PushBias, Trials: trials, MaxDays: maxDays, Victors: map[byte]int{}}
	for t := 0; t < trials; t++ {
		tr := RunTrial(downscale, p, maxDays, r)
		if tr.FirstHQDay >= 0 {
			s.FirstHQCount++
			s.FirstHQDays = append(s.FirstHQDays, tr.FirstHQDay)
		}
		if tr.VictorDay >= 0 {
			s.VictorCount++
			s.VictorDays = append(s.VictorDays, tr.VictorDay)
			s.Victors[tr.Victor]++
		}
	}
	return s
}
