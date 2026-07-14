package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Battle-report (mission-result) submission + war-state ingest.
//
// Reverse-engineered from Release.xex: builder sub_823BEF10 (internal message code 194) sends a fixed
// 565-byte binary report to port 1020+194 = 1214 (retail 1254); assembler sub_8217D4D0; response parser
// sub_823BD940. Only the mission HOST posts, exactly once per battle, WIN OR LOSE, and the report is an
// objective record of both squads -- so ingest is single-shot and the winner must be read from the body,
// never inferred from the sender. Full field map + accumulation design: SQUAD_STATS_DESIGN.md.
//
// REQUEST body (565 bytes, after the 32-byte header) -- the fields we ingest:
//   +0x23 area id            +0x24 map id
//   +0x25 block-1 nation     +0x26 block-1 team id ("TM.." or an AI placeholder like "BBB9999..")
//   +0x11C block-1 (team-0) winner-merit slot
//   +0x12C block-2 nation    +0x12D block-2 team id
//   +0x223 block-2 (team-1) winner-merit slot
//   +0x233 WINNER nation char   +0x234 occupation delta v92 (0..255, ~100 on a clean win)
// The winner nation matches exactly one block; that block's team won and its merit slot holds the earned
// renown (the loser's slot is 0 even when the loser was active). ACV/hound/COMBAS breakdowns are NOT in
// the report (they live only in local xstorage), so the squad leaderboard sources entirely from here.
//
// RESPONSE body is an ASCII CSV status read by sub_823BD940 at even offsets:
//   body[0] '1' = "Normal End"   body[2] '1' = battlefield-captured   body[4] '1' = area-captured
//   body[6] '1' = nation-defeated.  We accept the report ("1") and raise no special conquest events yet
//   ("1,0,0,0"); the occupation change is persisted so the war map reflects it on the next query.

// BattleReportAck is header(32) + a 7-byte CSV status body. Total = constants.BattleReportResponseSize (39).
type BattleReportAck struct {
	Header      MessageHeader
	Status      byte // off 0 - '1' = Normal End
	Sep1        byte // off 1 - ',' separator (ignored by parser)
	BattleField byte // off 2 - '1' => battlefield-captured event
	Sep2        byte // off 3 - ','
	Area        byte // off 4 - '1' => area-captured event
	Sep3        byte // off 5 - ','
	Nation      byte // off 6 - '1' => nation-defeated event
}

func CreateBattleReportAck(xuid [16]byte, order [8]byte) BattleReportAck {
	return BattleReportAck{
		Header: CreateHeader(xuid, order),
		Status: '1', Sep1: ',', BattleField: '0', Sep2: ',', Area: '0', Sep3: ',', Nation: '0',
	}
}

const battleReportBodySize = 565

// BattleResult is the ingestable outcome extracted from a 1254 report.
type BattleResult struct {
	AreaID, MapID             byte
	WinnerNation, LoserNation byte
	WinnerTeam, LoserTeam     string
	OccDelta                  int32 // war-map occupation gained by the winner (== "Capture Points")
	WinnerMerit               int32 // winner's earned merit (== the squad "Renown" gain)
}

// parseBattleReport pulls the outcome fields from a 565-byte report body. Returns false if the packet is
// too short or the winner nation matches neither participating block.
func parseBattleReport(packet []byte) (BattleResult, bool) {
	off := constants.MinHelloMessageSize
	if len(packet) < off+battleReportBodySize {
		return BattleResult{}, false
	}
	b := packet[off:]
	nationA, teamA := b[0x25], trimNullString(b[0x26:0x26+20])
	nationB, teamB := b[0x12C], trimNullString(b[0x12D:0x12D+20])

	r := BattleResult{
		AreaID:       b[0x23],
		MapID:        b[0x24],
		WinnerNation: b[0x233],
		OccDelta:     int32(b[0x234]),
	}
	switch r.WinnerNation {
	case nationA:
		r.WinnerTeam, r.LoserTeam, r.LoserNation = teamA, teamB, nationB
		r.WinnerMerit = int32(b[0x11C]) // team-0 merit slot
	case nationB:
		r.WinnerTeam, r.LoserTeam, r.LoserNation = teamB, teamA, nationA
		r.WinnerMerit = int32(b[0x223]) // team-1 merit slot
	default:
		return r, false
	}
	return r, true
}

type battleReportServer struct {
	*messageServer
	squadRepo *SquadRepository // nil when Mongo is disabled -> ack only, no accumulation
	worldRepo *WorldRepository
	// generateEvents mirrors the EventServers config toggle: when false we still apply occupation and squad
	// stats, but record NO data-driven world events (captures/dissolutions), leaving only the seeded briefing.
	generateEvents bool
}

// ingest persists one battle report: it moves the fought battlefield's occupation toward the winner and
// credits the winning squad's capture-points + renown. Best-effort -- failures are logged, never block
// the ack (the client must always get its response or it retries and the mission stalls).
func (s *battleReportServer) ingest(packet []byte) {
	res, ok := parseBattleReport(packet)
	if !ok {
		logging.Warn.Printf("[%s] unparseable battle report, acking without ingest", s.serverConfig.Label)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()

	if s.worldRepo != nil {
		bf, err := s.worldRepo.BattlefieldByAreaMap(ctx, res.AreaID, res.MapID)
		switch {
		case err != nil:
			logging.Warn.Printf("[%s] battlefield %d/%d read failed: %v", s.serverConfig.Label, res.AreaID, res.MapID, err)
		case bf == nil:
			logging.Warn.Printf("[%s] battle report for unknown battlefield %d/%d, acking without ingest", s.serverConfig.Label, res.AreaID, res.MapID)
		case bf.Locked:
			// A locked battlefield accepts no missions, so a report for one is spurious: ignore it
			// entirely (no occupation, no counters, no squad renown) rather than let a stray packet
			// overwrite the capture that locked it.
			logging.Info.Printf("[%s] battlefield %d/%d locked (defeated %s, unlock@%d); ignoring report", s.serverConfig.Label, res.AreaID, res.MapID, bf.DefeatedNation, bf.UnlockAtBattle)
			return
		default:
			s.applyBattle(ctx, res, *bf)
		}
	}
	if s.squadRepo != nil {
		if err := s.squadRepo.CreditBattle(ctx, res.WinnerTeam, res.LoserTeam, res.OccDelta, res.WinnerMerit, currentSeason); err != nil {
			logging.Warn.Printf("[%s] squad-stat credit failed: %v", s.serverConfig.Label, err)
		}
	}
	logging.Info.Printf("[%s] area %d/%d: winner %c/%s (+%d occ, +%d renown) vs %c/%s",
		s.serverConfig.Label, res.AreaID, res.MapID, res.WinnerNation, res.WinnerTeam, res.OccDelta, res.WinnerMerit, res.LoserNation, res.LoserTeam)
}

// applyBattle applies one battle report to the world under the winner-takes-all capture model:
//   - both combatants' battle counters advance (the unlock clock for locked battlefields);
//   - a change of holder flips the fought battlefield 100% to the winner and locks out the loser;
//   - if that flips the area's owner, the whole area is surrendered (every battlefield flips + locks);
//   - reaching a nation's unlock threshold reopens the battlefields it had lost.
//
// A successful defence (the winner already held the battlefield) changes no occupation and issues no
// lock -- only the counters advance. Capture/region/conquest events fire only on a genuine change of
// hands.
func (s *battleReportServer) applyBattle(ctx context.Context, res BattleResult, bf Battlefield) {
	beforeLead := bfLead(bf)
	changedHands := beforeLead != 0 && res.WinnerNation != beforeLead

	// Area owner before the battle -- needed to detect an area surrender and to fire region events.
	var before map[byte]int
	var beforeOwner byte
	if s.generateEvents {
		before, _ = s.worldRepo.NationAreaCounts(ctx)
		beforeOwner, _, _ = s.worldRepo.AreaAndBFLead(ctx, int32(res.AreaID), int32(res.MapID))
	}

	// Both nations fought a battle: advance their counters. This is the "10 other battles across all
	// areas" clock that eventually reopens battlefields each has lost.
	winnerCount, err := s.worldRepo.IncrementBattleCount(ctx, res.WinnerNation)
	if err != nil {
		logging.Warn.Printf("[%s] battle-count bump (winner %c) failed: %v", s.serverConfig.Label, res.WinnerNation, err)
	}
	loserCount, err := s.worldRepo.IncrementBattleCount(ctx, res.LoserNation)
	if err != nil {
		logging.Warn.Printf("[%s] battle-count bump (loser %c) failed: %v", s.serverConfig.Label, res.LoserNation, err)
	}

	if changedHands {
		// Flip the battlefield to the winner and lock out the former holder until it fights
		// UnlockBattleThreshold more battles. The former holder is normally the losing combatant.
		defeated := beforeLead
		defeatedCount := loserCount
		if defeated != res.LoserNation {
			defeatedCount, _ = s.worldRepo.NationBattleCount(ctx, defeated)
		}
		if err := s.worldRepo.CaptureBattlefield(ctx, int32(res.AreaID), int32(res.MapID), res.WinnerNation, defeated, defeatedCount+UnlockBattleThreshold); err != nil {
			logging.Warn.Printf("[%s] capture flip %d/%d failed: %v", s.serverConfig.Label, res.AreaID, res.MapID, err)
		}
	}

	// This battle may have carried either nation past a lock it was waiting out.
	if err := s.worldRepo.UnlockExpiredBattlefields(ctx, res.WinnerNation, winnerCount); err != nil {
		logging.Warn.Printf("[%s] unlock sweep (%c) failed: %v", s.serverConfig.Label, res.WinnerNation, err)
	}
	if err := s.worldRepo.UnlockExpiredBattlefields(ctx, res.LoserNation, loserCount); err != nil {
		logging.Warn.Printf("[%s] unlock sweep (%c) failed: %v", s.serverConfig.Label, res.LoserNation, err)
	}

	if !s.generateEvents {
		return
	}

	// Credit the winning squad's ledger (backs the |s2= "top squad of the capturing nation"), for
	// defences as well as captures so the ranking reflects all of a nation's activity in the area.
	if err := s.worldRepo.CreditCapture(ctx, int32(res.AreaID), int32(res.MapID), res.WinnerTeam, string(res.WinnerNation), res.OccDelta); err != nil {
		logging.Warn.Printf("[%s] capture-ledger credit failed: %v", s.serverConfig.Label, err)
	}
	if !changedHands {
		return // a defence flips nothing -> no capture/region/conquest events
	}

	s.maybeRecordConquest(ctx, res, before)
	afterOwner, afterLead, _ := s.worldRepo.AreaAndBFLead(ctx, int32(res.AreaID), int32(res.MapID))

	// Area surrender: this capture flipped the area's owner, so the losing nation gives up the whole
	// area -- every battlefield flips to the new owner and locks against the surrendering nation.
	if beforeOwner != 0 && afterOwner != 0 && beforeOwner != afterOwner {
		surCount, _ := s.worldRepo.NationBattleCount(ctx, beforeOwner)
		if err := s.worldRepo.FlipAndLockArea(ctx, int32(res.AreaID), afterOwner, beforeOwner, surCount+UnlockBattleThreshold); err != nil {
			logging.Warn.Printf("[%s] area surrender flip (area %d) failed: %v", s.serverConfig.Label, res.AreaID, err)
		}
	}

	s.maybeRecordCaptures(ctx, res, beforeOwner, beforeLead, afterOwner, afterLead)
}

// dissolutionRow maps an eliminated nation + its conqueror to the exact "Nation Dissolved" param row in
// WorldSituationInfoNewsParam.bin -- there is one row per loser/winner pair, so the rendered story names
// the TRUE conqueror (no more hardcoded-winner limitation). loser/winner are nation chars 'A'/'B'/'C'
// (Tarakia/Morskoj/Sal Kar). Returns false for an unknown pair (e.g. loser == winner).
func dissolutionRow(loser, winner byte) (int32, bool) {
	switch [2]byte{loser, winner} {
	case [2]byte{'A', 'B'}:
		return 3, true // Xeres / Tarakia -> Morskoj
	case [2]byte{'A', 'C'}:
		return 5, true // Xeres / Tarakia -> Sal Kar
	case [2]byte{'B', 'A'}:
		return 1, true // Ostrov / Morskoj -> Tarakia
	case [2]byte{'B', 'C'}:
		return 6, true // Ostrov / Morskoj -> Sal Kar
	case [2]byte{'C', 'A'}:
		return 2, true // Qara / Sal Kar -> Tarakia
	case [2]byte{'C', 'B'}:
		return 4, true // Qara / Sal Kar -> Morskoj
	}
	return 0, false
}

// isHQ reports whether an area is a nation capital (Xeres=1/Tarakia, Ostrov=2/Morskoj, Qara=3/Sal Kar).
// A capital falling is a dissolution/unification, not a region-capture event.
func isHQ(areaID byte) bool { return areaID == 1 || areaID == 2 || areaID == 3 }

// regionCaptureRow / battlefieldCaptureRow map the LOSER nation (abandoning the region/battlefield) to
// its WORLD_NEWS param row. These stories don't name the conqueror, so one row per loser suffices.
func regionCaptureRow(loser byte) (int32, bool) {
	switch loser {
	case 'B':
		return 29, true // Morskoj Abandons |a1=
	case 'C':
		return 30, true // Sal Kar Abandons |a1=
	case 'A':
		return 31, true // Tarakia Abandons |a1=
	}
	return 0, false
}

func battlefieldCaptureRow(loser byte) (int32, bool) {
	switch loser {
	case 'B':
		return 35, true // Morskoj Surrenders Battlefield
	case 'C':
		return 36, true // Sal Kar Surrenders Battlefield
	case 'A':
		return 37, true // Tarakia Surrenders Battlefield
	}
	return 0, false
}

// squadDisplayName resolves a wire team id (e.g. "TM0001000000000042", read from the battle report at
// +0x26) to the squad's human-readable name for the |s2= "most-involved squad" news slot. The capture
// ledger keys on the stable team id; we resolve to the name only at render time. Falls back to the raw id
// when the squad is unknown (e.g. an AI/placeholder team) or the squad repo is unavailable, so the slot is
// never blank/worse than before.
func (s *battleReportServer) squadDisplayName(ctx context.Context, teamID string) string {
	if teamID == "" || s.squadRepo == nil {
		return teamID
	}
	sq, err := s.squadRepo.SquadByTeamID(ctx, teamID)
	if err != nil {
		logging.Warn.Printf("[%s] squad-name lookup for %q failed: %v", s.serverConfig.Label, teamID, err)
		return teamID
	}
	if sq == nil || sq.Name == "" {
		return teamID
	}
	return sq.Name
}

// maybeRecordCaptures emits a battlefield-capture and/or a (non-HQ) region-capture WORLD_NEWS event when
// this battle changed the lead nation of the fought battlefield / area.
//
// Slot2 = the top-contributing squad NAME (|s2= renders it as a literal string). Slot1 = the place-name
// TEXT ID (NOT the name string): the region (|A1=/|a1=) and battlefield (|B1) placeholders are id-lookups --
// the client reads slot1 as a MenuText id and resolves the name itself (region ids 5020-5044, battlefield
// ids 5100-5209, from WorldSituationInfoNews{Area,Field}Param.bin). Writing the raw name string here renders
// blank; the id must be sent. areaNameSlot/battlefieldNameSlot yield that id as ASCII decimal.
func (s *battleReportServer) maybeRecordCaptures(ctx context.Context, res BattleResult, beforeOwner, beforeLead, afterOwner, afterLead byte) {
	now := time.Now().Unix()
	// Battlefield captured: the fought battlefield's lead nation changed.
	if beforeLead != 0 && afterLead != 0 && beforeLead != afterLead {
		teamID, _ := s.worldRepo.TopSquadForBattlefield(ctx, int32(res.AreaID), int32(res.MapID), string(afterLead))
		squad := s.squadDisplayName(ctx, teamID)
		if row, err := RecordBattlefieldCaptureEvent(ctx, s.worldRepo, int32(res.AreaID), int32(res.MapID), beforeLead, afterLead, squad, now); err != nil {
			logging.Warn.Printf("[%s] record battlefield-capture event failed: %v", s.serverConfig.Label, err)
		} else if row != 0 {
			logging.Info.Printf("[%s] EVENT: battlefield %q (%d/%d) captured %c->%c (squad %q) -> row %d", s.serverConfig.Label, battlefieldName(int32(res.AreaID), int32(res.MapID)), res.AreaID, res.MapID, beforeLead, afterLead, squad, row)
		}
	}
	// Region captured: the AREA's owner changed (RecordRegionCaptureEvent skips HQ flips = dissolution).
	if beforeOwner != 0 && afterOwner != 0 && beforeOwner != afterOwner {
		teamID, _ := s.worldRepo.TopSquadForRegion(ctx, int32(res.AreaID), string(afterOwner))
		squad := s.squadDisplayName(ctx, teamID)
		if row, err := RecordRegionCaptureEvent(ctx, s.worldRepo, int32(res.AreaID), beforeOwner, afterOwner, squad, now); err != nil {
			logging.Warn.Printf("[%s] record region-capture event failed: %v", s.serverConfig.Label, err)
		} else if row != 0 {
			logging.Info.Printf("[%s] EVENT: region %q (%d) captured %c->%c (squad %q) -> row %d", s.serverConfig.Label, areaName(int32(res.AreaID)), res.AreaID, beforeOwner, afterOwner, squad, row)
		}
	}
}

// RecordBattlefieldCaptureEvent writes a "X Surrenders Battlefield" WORLD_NEWS event when a battlefield's
// lead nation changed (beforeLead -> afterLead, both non-zero and different). squad is the pre-resolved |s2=
// name (may be ""). Returns the param row written (0 = no event). Shared by the battle-report ingest and the
// admin capture tool (cmd/capture) so both produce identical news.
func RecordBattlefieldCaptureEvent(ctx context.Context, repo *WorldRepository, areaID, mapID int32, beforeLead, afterLead byte, squad string, now int64) (int32, error) {
	if beforeLead == 0 || afterLead == 0 || beforeLead == afterLead {
		return 0, nil
	}
	row, ok := battlefieldCaptureRow(beforeLead)
	if !ok {
		return 0, nil
	}
	// slot1 = |B1 battlefield name TEXT ID (client resolves it), slot2 = |s2= most-involved squad.
	ev := EventRecord{CreatedAt: now, TemplateID: row, Slot1: battlefieldNameSlot(areaID, mapID), Slot2: squad}
	if err := repo.RecordEvent(ctx, ev); err != nil {
		return 0, err
	}
	return row, nil
}

// RecordRegionCaptureEvent writes an "X Abandons <region>" WORLD_NEWS event when a NON-HQ area's owner
// changed (beforeOwner -> afterOwner). squad = |s2=. Returns the row written (0 = no event; HQ flips return 0
// -- those are a dissolution, handled separately). Shared by the battle-report ingest and cmd/capture.
func RecordRegionCaptureEvent(ctx context.Context, repo *WorldRepository, areaID int32, beforeOwner, afterOwner byte, squad string, now int64) (int32, error) {
	if beforeOwner == 0 || afterOwner == 0 || beforeOwner == afterOwner || isHQ(byte(areaID)) {
		return 0, nil
	}
	row, ok := regionCaptureRow(beforeOwner)
	if !ok {
		return 0, nil
	}
	// slot1 = |A1= region name TEXT ID (client resolves it), slot2 = |s2= most-involved squad.
	ev := EventRecord{CreatedAt: now, TemplateID: row, Slot1: areaNameSlot(areaID), Slot2: squad}
	if err := repo.RecordEvent(ctx, ev); err != nil {
		return 0, err
	}
	return row, nil
}

// maybeRecordConquest emits a nation-dissolved world event when this battle eliminated the losing nation
// (its controlled-area count went from >0 to 0). before is the per-nation area count captured just before
// the occupation update; a nil before or a failed post-count skips detection rather than blocking the ack.
func (s *battleReportServer) maybeRecordConquest(ctx context.Context, res BattleResult, before map[byte]int) {
	if before == nil {
		return
	}
	after, err := s.worldRepo.NationAreaCounts(ctx)
	if err != nil {
		logging.Warn.Printf("[%s] post-battle area count failed: %v", s.serverConfig.Label, err)
		return
	}
	if before[res.LoserNation] == 0 || after[res.LoserNation] != 0 {
		return // not a fresh elimination
	}
	row, ok := dissolutionRow(res.LoserNation, res.WinnerNation)
	if !ok {
		return
	}
	ev := EventRecord{CreatedAt: time.Now().Unix(), TemplateID: row}
	if err := s.worldRepo.RecordEvent(ctx, ev); err != nil {
		logging.Warn.Printf("[%s] record dissolution event failed: %v", s.serverConfig.Label, err)
		return
	}
	logging.Info.Printf("[%s] EVENT: nation %c eliminated by %c -> news row %d", s.serverConfig.Label, res.LoserNation, res.WinnerNation, row)
}

func NewBattleReportServer(listenAddress net.IP, serverConfig config.EventServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, squadRepo *SquadRepository, worldRepo *WorldRepository) *battleReportServer {
	s := &battleReportServer{squadRepo: squadRepo, worldRepo: worldRepo, generateEvents: serverConfig.GenerateEvents}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig.ServerConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateWorldPacket(packet, clientAddr, serverConfig.Label)
		},
		// buildResponse (not buildPayload) so we can read the full report body for ingest.
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			s.ingest(*readBuffer)
			buf := make([]byte, constants.BattleReportResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, CreateBattleReportAck(hi.Xuid, hi.Order)); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
