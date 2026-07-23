// Command loki-calibrate derives the sim's nation-strength calibration from production combas battle logs in
// Loki, and writes it as JSON for cmd/predict (-calibration). It reads the ingested battle-report lines
// ("area A/M: winner N/T (+D occ, +R renown) vs N/T"), then computes per-nation PvP win probabilities
// (WinProb) and attack-initiation weights (Activity), plus the battle rate, PvP fraction, CPU win rate and
// the PvP / CPU-loss occupation-delta histograms.
//
// Connection + scope come from flags or env:
//
//	LOKI_URL / -url            Loki base, e.g. https://monitoring.example
//	LOKI_USERNAME / -user      basic-auth user   (optional)
//	LOKI_PASSWORD / -pass      basic-auth pass   (optional)
//	-env                       value of the `environment` stream label to filter (e.g. production)
//	-container                 container label (default opencombas-combas-server-1)
//	-since                     lookback window: Go duration or Nd (e.g. 720h, 30d)
//	-out                       output file (default stdout)
//
// Example:
//
//	LOKI_PASSWORD=… go run ./cmd/loki-calibrate -url https://monitoring.example -user promtail \
//	    -env production -since 30d -out cal.json
//	go run ./cmd/predict -calibration cal.json -downscale 20,40 -cpudownscale 0.1,0.5
package main

import (
	"ChromehoundsStatusServer/predict"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")
	line = regexp.MustCompile(`area (\d+)/(\d+): winner (\w)/(\S+) \(\+(\d+) occ, \+(\d+) renown\) vs (\w)/(\S+)`)
)

// parseSince accepts a Go duration or an "Nd" (days) shorthand.
func parseSince(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func envOr(flagVal, key string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(key)
}

// queryRange runs one Loki query_range and returns the raw log lines.
func queryRange(base, user, pass, logql string, start, end time.Time) ([]string, error) {
	q := url.Values{}
	q.Set("query", logql)
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("limit", "5000")
	q.Set("direction", "forward")
	req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("loki %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	var lines []string
	for _, s := range out.Data.Result {
		for _, v := range s.Values {
			lines = append(lines, v[1])
		}
	}
	return lines, nil
}

func main() {
	urlF := flag.String("url", "", "Loki base URL (or LOKI_URL)")
	userF := flag.String("user", "", "basic-auth user (or LOKI_USERNAME)")
	passF := flag.String("pass", "", "basic-auth password (or LOKI_PASSWORD)")
	env := flag.String("env", "production", "value of the `environment` stream label to filter")
	container := flag.String("container", "opencombas-combas-server-1", "combas-server container label")
	since := flag.String("since", "30d", "lookback window (Go duration or Nd, e.g. 720h / 30d)")
	out := flag.String("out", "", "output file (default stdout)")
	flag.Parse()

	base := envOr(*urlF, "LOKI_URL")
	if base == "" {
		fmt.Fprintln(os.Stderr, "error: set -url or LOKI_URL")
		os.Exit(1)
	}
	user, pass := envOr(*userF, "LOKI_USERNAME"), envOr(*passF, "LOKI_PASSWORD")
	dur, err := parseSince(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bad -since %q: %v\n", *since, err)
		os.Exit(1)
	}

	logql := fmt.Sprintf("{environment=%q,container=%q} |= ` occ, +`", *env, *container)
	end := time.Now()
	start := end.Add(-dur)

	// Query in 7-day chunks so no single request risks Loki's per-query line cap.
	var lines []string
	const chunk = 7 * 24 * time.Hour
	for t := start; t.Before(end); t = t.Add(chunk) {
		te := t.Add(chunk)
		if te.After(end) {
			te = end
		}
		ls, err := queryRange(base, user, pass, logql, t, te)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: loki query [%s..%s]: %v\n", t.Format("01-02"), te.Format("01-02"), err)
			os.Exit(1)
		}
		lines = append(lines, ls...)
	}

	// Tally.
	var total, pvp, cpuWin, cpuLoss int
	matchup := map[[2]byte]int{} // winner,loser
	activity := map[byte]int{}   // PvP participation
	pvpDelta := map[string]int{}
	cpuLossDelta := map[string]int{}
	for _, l := range lines {
		m := line.FindStringSubmatch(ansi.ReplaceAllString(l, ""))
		if m == nil {
			continue
		}
		total++
		wn, wt, ln, lt := m[3][0], m[4], m[7][0], m[8]
		od := m[5]
		realW, realL := strings.HasPrefix(wt, "TM"), strings.HasPrefix(lt, "TM")
		switch {
		case realW && realL: // PvP
			pvp++
			matchup[[2]byte{wn, ln}]++
			activity[wn]++
			activity[ln]++
			pvpDelta[od]++
		case realW: // real squad beat a CPU
			cpuWin++
		default: // real squad lost to a CPU (drain)
			cpuLoss++
			cpuLossDelta[od]++
		}
	}
	if total == 0 {
		fmt.Fprintln(os.Stderr, "error: no battle-report lines matched -- check -env/-container/-since and the log format")
		os.Exit(1)
	}

	cal := predict.Calibration{
		Source:       fmt.Sprintf("loki %s env=%s container=%s since=%s (%s..%s)", base, *env, *container, *since, start.Format("2006-01-02"), end.Format("2006-01-02")),
		Battles:      total,
		RatePerDay:   float64(total) / dur.Hours() * 24,
		PvPFrac:      float64(pvp) / float64(total),
		WinProb:      map[string]map[string]float64{},
		Activity:     map[string]float64{},
		PvPDelta:     pvpDelta,
		CPULossDelta: cpuLossDelta,
	}
	if cpuWin+cpuLoss > 0 {
		cal.CPUWinRate = float64(cpuWin) / float64(cpuWin+cpuLoss)
	}
	for _, x := range []byte{'A', 'B', 'C'} {
		cal.Activity[string(x)] = float64(activity[x])
		row := map[string]float64{}
		for _, y := range []byte{'A', 'B', 'C'} {
			if x == y {
				continue
			}
			w, l := matchup[[2]byte{x, y}], matchup[[2]byte{y, x}]
			if w+l > 0 {
				row[string(y)] = float64(w) / float64(w+l)
			}
		}
		if len(row) > 0 {
			cal.WinProb[string(x)] = row
		}
	}

	data, _ := json.MarshalIndent(cal, "", "  ")
	data = append(data, '\n')
	if *out == "" {
		os.Stdout.Write(data)
	} else if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "calibrated from %d battles (%d PvP, %d CPU-win, %d CPU-loss) over %s -> %s\n",
		total, pvp, cpuWin, cpuLoss, *since, orStdout(*out))
}

func orStdout(s string) string {
	if s == "" {
		return "stdout"
	}
	return s
}
