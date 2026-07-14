// Command simulate drives synthetic battle reports through the live server's BattleApplier against the
// world DB. Unlike cmd/capture (which sets occupation state directly), this runs the REAL game logic --
// winner-takes-all captures, battlefield/area locks, the 10-battle unlock clock, area cascades, HQ
// dissolution + revival lockout, and the WORLD_NEWS events they raise -- so you can exercise and verify
// behaviour without grinding missions or sending UDP. It shares the server's one code path, so it can
// never drift from what the live server actually does.
//
// A "battle" is: attacker vs defender on one battlefield, and who won. Attacker-wins is a capture attempt
// (flips the battlefield if the attacker wasn't already the holder); -defense means the holder repelled
// it (no flip, only the battle counters advance).
//
//	go run ./cmd/simulate -area 2 -map 1 -attacker A -defender B           # A attacks B's capital bf; A wins
//	go run ./cmd/simulate -area 5 -map 1 -attacker A -defender B -defense  # B repels A (successful defence)
//	go run ./cmd/simulate -scenario scenarios/hq_fall.txt                  # replay a whole sequence
//
// Scenario file: one battle per line, "# ..." and blank lines ignored:
//
//	area map attacker defender outcome [winnerTeam] [occ]
//	2 1 A B win  TM0000000000000001 100
//	2 2 A B win
//	2 1 A B defense
//
// The world must already be seeded (run cmd/reset first); simulate never creates battlefields.
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/server"
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// synthetic default team ids (no real names -- this is an open-source test tool).
const (
	defaultWinnerTeam = "TM0000000000000001"
	defaultLoserTeam  = "TM0000000000000002"
)

type sim struct {
	world   *server.WorldRepository
	applier *server.BattleApplier
}

func main() {
	area := flag.Int("area", 0, "area id (1-22)")
	mapID := flag.Int("map", 0, "battlefield id within the area")
	attacker := flag.String("attacker", "", "attacking nation: A | B | C")
	defender := flag.String("defender", "", "defending (current holder's) nation: A | B | C")
	defense := flag.Bool("defense", false, "the defender wins (mission repelled); default is the attacker wins/captures")
	winnerTeam := flag.String("squad", defaultWinnerTeam, "winner's team id (backs |s2= ledger + squad stats)")
	loserTeam := flag.String("loser-squad", defaultLoserTeam, "loser's team id")
	occ := flag.Int("occ", 100, "occupation/capture points credited to the winner")
	merit := flag.Int("merit", 100, "winner renown/merit")
	scenario := flag.String("scenario", "", "path to a scenario file (one battle per line); overrides the single-battle flags")
	flag.Parse()

	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[SIM] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[SIM] mongo connect failed: %v", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		_ = store.Close(closeCtx)
	}()

	world := server.NewWorldRepository(store)
	squad := server.NewSquadRepository(store)
	// generateEvents=true so the tool exercises the full event/dissolution path; label "SIM".
	s := &sim{world: world, applier: server.NewBattleApplier(world, squad, true, "SIM")}

	if *scenario != "" {
		s.runScenario(ctx, *scenario)
		return
	}

	res, err := buildResult(*area, *mapID, *attacker, *defender, *defense, *winnerTeam, *loserTeam, int32(*occ), int32(*merit))
	if err != nil {
		logging.Error.Fatalf("[SIM] %v", err)
	}
	s.runOne(ctx, res, true)
}

// buildResult validates the inputs and turns them into a BattleResult, deriving winner/loser from the
// attack outcome. defense=true means the defender (current holder) won.
func buildResult(area, mapID int, attacker, defender string, defense bool, winnerTeam, loserTeam string, occ, merit int32) (server.BattleResult, error) {
	an, ok := nationChar(attacker)
	if !ok {
		return server.BattleResult{}, fmt.Errorf("-attacker must be A, B, or C (got %q)", attacker)
	}
	dn, ok := nationChar(defender)
	if !ok {
		return server.BattleResult{}, fmt.Errorf("-defender must be A, B, or C (got %q)", defender)
	}
	if an == dn {
		return server.BattleResult{}, fmt.Errorf("attacker and defender must differ (both %c)", an)
	}
	if area < 1 || area > 22 {
		return server.BattleResult{}, fmt.Errorf("-area must be 1-22 (got %d)", area)
	}
	if mapID < 1 {
		return server.BattleResult{}, fmt.Errorf("-map must be >= 1 (got %d)", mapID)
	}
	res := server.BattleResult{
		AreaID: byte(area), MapID: byte(mapID),
		WinnerTeam: winnerTeam, LoserTeam: loserTeam,
		OccDelta: occ, WinnerMerit: merit,
	}
	if defense {
		res.WinnerNation, res.LoserNation = dn, an // holder repelled the attacker
	} else {
		res.WinnerNation, res.LoserNation = an, dn // attacker captured
	}
	return res, nil
}

// runOne applies a single battle. When full is true it prints the target area's before/after state; it
// always prints the news events the battle raised. Whether the fought battlefield actually flipped (a
// capture) or held (a defence) is visible in the BEFORE/AFTER lead.
func (s *sim) runOne(ctx context.Context, res server.BattleResult, full bool) {
	fmt.Printf("=== area %d/%d: winner %c/%s  vs loser %c/%s ===\n",
		res.AreaID, res.MapID, res.WinnerNation, res.WinnerTeam, res.LoserNation, res.LoserTeam)
	if full {
		s.printArea(ctx, "BEFORE", int32(res.AreaID))
	}
	beforeN := s.eventCount(ctx)
	s.applier.Apply(ctx, res)
	if full {
		s.printArea(ctx, "AFTER", int32(res.AreaID))
		s.printNations(ctx)
	} else {
		// Scenario mode: show the fought battlefield's occupation so the accumulation -> crossover -> capture
		// progression is visible mission by mission, plus the fought area's running PvP share / fierce flag.
		s.printFoughtBf(ctx, res)
		s.printAreaFierce(ctx, int32(res.AreaID))
	}
	s.printNewEvents(ctx, beforeN)
	fmt.Println()
}

// printAreaFierce prints the fought area's running PvP share and fierce-battle flag (>30% PvP). Shown even
// at 0/0 so a flip's stat reset is visible.
func (s *sim) printAreaFierce(ctx context.Context, area int32) {
	total, pvp, err := s.world.AreaBattleCounts(ctx, area)
	if err != nil {
		return
	}
	pct := int64(0)
	if total > 0 {
		pct = pvp * 100 / total
	}
	status := "calm"
	if pvp*100 > total*30 {
		status = "FIERCE"
	}
	fmt.Printf("  area %d  PvP %d/%d (%d%%)  -> %s\n", area, pvp, total, pct, status)
}

// printFoughtBf prints the just-fought battlefield's occupation, lead, and lock state.
func (s *sim) printFoughtBf(ctx context.Context, res server.BattleResult) {
	bf, err := s.world.BattlefieldByAreaMap(ctx, res.AreaID, res.MapID)
	if err != nil || bf == nil {
		return
	}
	lock := ""
	if bf.Locked {
		lock = fmt.Sprintf("   LOCKED(vs %s, unlock@%d)", bf.DefeatedNation, bf.UnlockAtBattle)
	}
	fmt.Printf("  bf %d/%d  A=%-6d B=%-6d C=%-6d  -> %c%s\n",
		bf.AreaID, bf.MapID, bf.OccA, bf.OccB, bf.OccC, leadChar(*bf), lock)
}

func (s *sim) runScenario(ctx context.Context, path string) {
	f, err := os.Open(path)
	if err != nil {
		logging.Error.Fatalf("[SIM] open scenario %q: %v", path, err)
	}
	defer f.Close()

	touched := map[int32]bool{}
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		res, err := parseScenarioLine(text)
		if err != nil {
			logging.Error.Fatalf("[SIM] scenario line %d (%q): %v", line, text, err)
		}
		touched[int32(res.AreaID)] = true
		s.runOne(ctx, res, false) // compact per-step: battle header + events, no full dump
	}
	if err := scanner.Err(); err != nil {
		logging.Error.Fatalf("[SIM] read scenario: %v", err)
	}

	fmt.Println("=== final state ===")
	for area := range touched {
		s.printArea(ctx, fmt.Sprintf("area %d", area), area)
	}
	s.printNations(ctx)
}

// parseScenarioLine parses "area map attacker defender outcome [winnerTeam] [occ]".
func parseScenarioLine(text string) (server.BattleResult, error) {
	f := strings.Fields(text)
	if len(f) < 5 {
		return server.BattleResult{}, fmt.Errorf("want at least: area map attacker defender outcome")
	}
	area, err := strconv.Atoi(f[0])
	if err != nil {
		return server.BattleResult{}, fmt.Errorf("area: %v", err)
	}
	mapID, err := strconv.Atoi(f[1])
	if err != nil {
		return server.BattleResult{}, fmt.Errorf("map: %v", err)
	}
	defense, err := outcomeIsDefense(f[4])
	if err != nil {
		return server.BattleResult{}, err
	}
	winnerTeam, loserTeam := defaultWinnerTeam, defaultLoserTeam
	if len(f) >= 6 {
		winnerTeam = f[5]
	}
	occ := int32(100)
	if len(f) >= 7 {
		n, err := strconv.Atoi(f[6])
		if err != nil {
			return server.BattleResult{}, fmt.Errorf("occ: %v", err)
		}
		occ = int32(n)
	}
	return buildResult(area, mapID, f[2], f[3], defense, winnerTeam, loserTeam, occ, 100)
}

func outcomeIsDefense(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "win", "capture", "attacker":
		return false, nil
	case "defense", "defence", "hold", "defender":
		return true, nil
	}
	return false, fmt.Errorf("outcome %q must be win|defense", s)
}

func (s *sim) printArea(ctx context.Context, header string, area int32) {
	fmt.Printf("%s:\n", header)
	bfs, err := s.world.BattlefieldsByArea(ctx, byte(area))
	if err != nil {
		fmt.Printf("  (read failed: %v)\n", err)
		return
	}
	for _, b := range bfs {
		lock := ""
		if b.Locked {
			lock = fmt.Sprintf("   LOCKED(vs %s, unlock@%d)", b.DefeatedNation, b.UnlockAtBattle)
		}
		fmt.Printf("  area %d map %d  A=%-6d B=%-6d C=%-6d  -> %c%s\n",
			b.AreaID, b.MapID, b.OccA, b.OccB, b.OccC, leadChar(b), lock)
	}
}

func (s *sim) printNations(ctx context.Context) {
	fmt.Println("nations:")
	for _, n := range []byte{'A', 'B', 'C'} {
		cnt, _ := s.world.NationBattleCount(ctx, n)
		captor, _ := s.world.NationHQLostTo(ctx, n)
		state := "active"
		if captor != 0 {
			state = fmt.Sprintf("DISSOLVED (capital lost to %c; HQ-only until revival)", captor)
		}
		fmt.Printf("  nation %c  battles=%d  %s\n", n, cnt, state)
	}
}

func (s *sim) eventCount(ctx context.Context) int {
	evs, _ := s.world.RecentEvents(ctx, 1000)
	return len(evs)
}

// printNewEvents prints the events appended since beforeN (RecentEvents is newest-first, so the new ones
// are at the front).
func (s *sim) printNewEvents(ctx context.Context, beforeN int) {
	evs, _ := s.world.RecentEvents(ctx, 1000)
	newN := len(evs) - beforeN
	if newN <= 0 {
		fmt.Println("  news: (none)")
		return
	}
	fmt.Printf("  news: %d event(s) fired\n", newN)
	for _, e := range evs[:newN] {
		fmt.Printf("    + row %d\n", e.TemplateID)
	}
}

func nationChar(s string) (byte, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "A":
		return 'A', true
	case "B":
		return 'B', true
	case "C":
		return 'C', true
	}
	return 0, false
}

func leadChar(b server.Battlefield) byte {
	best, ch := b.OccA, byte('A')
	if b.OccB > best {
		best, ch = b.OccB, 'B'
	}
	if b.OccC > best {
		best, ch = b.OccC, 'C'
	}
	if best == 0 {
		return '-'
	}
	return ch
}
