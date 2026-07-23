// Command predict is an OFFLINE season planner. It Monte-Carlo-simulates the war from the canonical reset
// state under a range of downscales and "push bias" (how aggressively fronts advance toward enemy capitals),
// and reports how often / how fast a single-nation outcome is reached. It touches no database and is not part
// of the live server -- it exists to sweep reset settings before committing a season.
//
// The battle model is calibrated from 30 days of production combas logs (rate, PvP/CPU mix, per-nation
// strength, CPU win/loss occupation deltas); the war kernel is a faithful port of server.BattleApplier. See
// package predict. Behaviour (PushBias, strengths) is uncertain -- SWEEP it and read the sensitivity, don't
// trust a single point estimate.
//
//	go run ./cmd/predict -downscale 20,40,80 -pushbias 0.25,0.5,1.0 -trials 200 -maxdays 60
package main

import (
	"ChromehoundsStatusServer/predict"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func floats(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if v, err := strconv.ParseFloat(p, 64); err == nil {
				out = append(out, v)
			}
		}
	}
	return out
}
func ints(s string) []int32 {
	var out []int32
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if v, err := strconv.Atoi(p); err == nil {
				out = append(out, int32(v))
			}
		}
	}
	return out
}

func main() {
	downscales := flag.String("downscale", "20,40,80,160", "comma-separated reset downscales to sweep")
	pushbiases := flag.String("pushbias", "0.25,0.5,1.0", "comma-separated push-bias values (0=churn the border, 1=always advance toward an enemy capital)")
	cpudownscales := flag.String("cpudownscale", "0.1", "comma-separated CPU-win downscales (0.1=current regime ~2 occ, 1.0=un-downscaled ~20; CPU-loss drain stays ~19 either way)")
	strengths := flag.String("strength", "1.0", "comma-separated nation-strength imbalance scales (0=parity/contested, 1=as-measured, >1=amplified)")
	trials := flag.Int("trials", 200, "Monte-Carlo trials per (downscale, pushbias)")
	maxdays := flag.Int("maxdays", 60, "season length cap in days (60 ~= the 2-month time-limit accord)")
	seed := flag.Int64("seed", 1, "RNG seed (fixed for reproducibility)")
	rate := flag.Float64("rate", 0, "override battles/day (0 = use the calibration / built-in default)")
	pvpfrac := flag.Float64("pvpfrac", 0, "override PvP fraction (0 = use the calibration / default ~0.096; retail-scale play would be much higher)")
	calFile := flag.String("calibration", "", "JSON calibration from cmd/loki-calibrate (nation strengths etc.); empty = built-in production default")
	flag.Parse()

	base := predict.DefaultParams()
	src := "built-in production default"
	if *calFile != "" {
		cal, err := predict.LoadCalibration(*calFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: load calibration %q: %v\n", *calFile, err)
			os.Exit(1)
		}
		base = cal.Params()
		src = fmt.Sprintf("%s (%d battles)", cal.Source, cal.Battles)
	}
	if *rate > 0 {
		base.RatePerDay = *rate
	}
	if *pvpfrac > 0 {
		base.PvPFrac = *pvpfrac
	}
	fmt.Printf("Calibration: %s\n", src)

	fmt.Printf("Season predictor -- %d trials/cell, %d-day cap, rate %.1f battles/day (PvP %.0f%%), seed %d\n",
		*trials, *maxdays, base.RatePerDay, base.PvPFrac*100, *seed)
	fmt.Printf("Victor = one nation holds all 3 capitals; FirstHQ = first capital falls. Days among trials that reached it.\n")
	fmt.Printf("cpuDS = CPU-win downscale (0.1 = current ~2 occ; higher = bigger CPU-win advance; CPU-loss drain fixed ~19).\n")
	fmt.Printf("str = nation-strength imbalance (0 = parity/contested, 1 = as-measured, >1 = amplified).\n\n")
	fmt.Printf("%-5s %-6s %-9s %-6s | %-9s %-22s | %-9s %-9s | %s\n",
		"str", "cpuDS", "downscale", "push", "victor%", "victor days (p10/med/p90)", "1stHQ%", "1stHQ med", "victor A/B/C")
	fmt.Println(strings.Repeat("-", 112))

	for _, ss := range floats(*strengths) {
		for _, cds := range floats(*cpudownscales) {
			for _, ds := range ints(*downscales) {
				for _, pb := range floats(*pushbiases) {
					p := base.WithCPUDownscale(cds).WithStrengthScale(ss)
					p.PushBias = pb
					s := predict.MonteCarlo(ds, p, *trials, *maxdays, *seed)
					vd := "-"
					if s.VictorCount > 0 {
						vd = fmt.Sprintf("%d / %d / %d", s.P10VictorDay(), s.MedVictorDay(), s.P90VictorDay())
					}
					hq := "-"
					if s.FirstHQCount > 0 {
						hq = strconv.Itoa(s.MedFirstHQDay())
					}
					fmt.Printf("%-5.2f %-6.2f %-9d %-6.2f | %-8.1f%% %-22s | %-8.1f%% %-9s | %d/%d/%d\n",
						ss, cds, ds, pb, 100*s.VictorFrac(), vd, 100*s.FirstHQFrac(), hq,
						s.Victors['A'], s.Victors['B'], s.Victors['C'])
				}
			}
		}
	}
}
