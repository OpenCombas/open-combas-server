package predict

import (
	"encoding/json"
	"os"
	"strconv"
)

// Calibration is the data-derived battle model, emitted by cmd/loki-calibrate and consumed by cmd/predict
// (via -calibration). Keys are nation chars as strings ("A"/"B"/"C"); delta histograms are keyed by the
// stringified occupation delta. Any zero/empty field falls back to the built-in production default, so a
// partial calibration (e.g. nation weights only) is valid.
type Calibration struct {
	Source     string                        `json:"source"`               // provenance (loki host, env, span)
	Battles    int                           `json:"battles"`              // battles observed
	RatePerDay float64                       `json:"ratePerDay,omitempty"` // battles/day over the span
	PvPFrac    float64                       `json:"pvpFrac,omitempty"`
	CPUWinRate float64                       `json:"cpuWinRate,omitempty"`
	// WinProb[X][Y] = P(X beats Y) in PvP, from head-to-head counts. THE nation-strength "who wins a fight".
	WinProb map[string]map[string]float64 `json:"winProb,omitempty"`
	// Activity[X] = X's PvP participation (attack-initiation weight). The nation-strength "who fights most".
	Activity     map[string]float64 `json:"activity,omitempty"`
	PvPDelta     map[string]int     `json:"pvpDelta,omitempty"`     // PvP occ-delta histogram
	CPULossDelta map[string]int     `json:"cpuLossDelta,omitempty"` // CPU-loss (drain) occ-delta histogram
}

func histDist(h map[string]int) *dist {
	m := map[int32]int{}
	for k, v := range h {
		if n, err := strconv.Atoi(k); err == nil {
			m[int32(n)] = v
		}
	}
	return newDist(m)
}

// Params folds the calibration onto the production DefaultParams (so unset fields keep the default, and the
// CPU-win downscale knob -- a planning lever, not a measured value -- is preserved).
func (c Calibration) Params() Params {
	p := DefaultParams()
	if c.RatePerDay > 0 {
		p.RatePerDay = c.RatePerDay
	}
	if c.PvPFrac > 0 {
		p.PvPFrac = c.PvPFrac
	}
	if c.CPUWinRate > 0 {
		p.CPUWinRate = c.CPUWinRate
	}
	if len(c.WinProb) > 0 {
		wp := map[byte]map[byte]float64{}
		for x, row := range c.WinProb {
			m := map[byte]float64{}
			for y, v := range row {
				m[y[0]] = v
			}
			wp[x[0]] = m
		}
		p.WinProb = wp
	}
	if len(c.Activity) > 0 {
		a := map[byte]float64{}
		for k, v := range c.Activity {
			a[k[0]] = v
		}
		p.Activity = a
	}
	if len(c.PvPDelta) > 0 {
		p.pvpDelta = histDist(c.PvPDelta)
	}
	if len(c.CPULossDelta) > 0 {
		p.cpuLoss = histDist(c.CPULossDelta)
	}
	return p
}

// LoadCalibration reads a calibration JSON file.
func LoadCalibration(path string) (Calibration, error) {
	var c Calibration
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}
