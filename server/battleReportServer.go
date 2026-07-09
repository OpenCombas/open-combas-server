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
		// Snapshot per-nation area control just before the update so we can detect an elimination
		// (loser drops from >0 controlled areas to 0) caused by this battle.
		before, _ := s.worldRepo.NationAreaCounts(ctx)
		if err := s.worldRepo.ApplyBattleOccupation(ctx, res.AreaID, res.MapID, res.WinnerNation, res.LoserNation, res.OccDelta); err != nil {
			logging.Warn.Printf("[%s] occupation update failed: %v", s.serverConfig.Label, err)
		} else {
			s.maybeRecordConquest(ctx, res, before)
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

// dissolutionTemplate maps an eliminated nation to its known "Nation Dissolved" WORLD_NEWS template.
// LIMITATION: each template hardcodes the CONQUEROR in its fixed client text (t3 Tarakia->Morskovian,
// t1 Morskoj->Tarakian, t2 SalKar->Tarakian), so the winner rendered may not match the actual conqueror
// until the other loser/winner-pairing templates are recovered. Returns false for an unknown nation.
func dissolutionTemplate(loser byte) (int32, bool) {
	switch loser {
	case 'A':
		return 3, true // Xeres / Tarakia falls
	case 'B':
		return 1, true // Ostrov / Morskoj falls
	case 'C':
		return 2, true // Qara / Sal Kar falls
	}
	return 0, false
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
	tid, ok := dissolutionTemplate(res.LoserNation)
	if !ok {
		return
	}
	ev := EventRecord{CreatedAt: time.Now().Unix(), TemplateID: tid, EntityID: int32(res.LoserNation), Text: ""}
	if err := s.worldRepo.RecordEvent(ctx, ev); err != nil {
		logging.Warn.Printf("[%s] record dissolution event failed: %v", s.serverConfig.Label, err)
		return
	}
	logging.Info.Printf("[%s] EVENT: nation %c eliminated by %c -> news template %d", s.serverConfig.Label, res.LoserNation, res.WinnerNation, tid)
}

func NewBattleReportServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, squadRepo *SquadRepository, worldRepo *WorldRepository) *battleReportServer {
	s := &battleReportServer{squadRepo: squadRepo, worldRepo: worldRepo}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig,
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
