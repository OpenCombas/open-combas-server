package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"encoding/binary"
	"net"
	"sync"

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
//   +0x4A block-1 pilot ids  6x "US.." (19 bytes each) -- the pilots who fought for block 1 (assembler
//                            sub_8217D4D0); +0xBC their gamertags (6x16). Empty slots => fewer than 6 fought.
//   +0x11C block-1 (team-0) winner-merit slot
//   +0x12C block-2 nation    +0x12D block-2 team id   +0x151 block-2 pilot ids (6x19)  +0x1C3 gamertags
//   +0x223 block-2 (team-1) winner-merit slot
//   +0x233 WINNER nation char   +0x234 occupation delta v92 (0..255, ~100 on a clean win)
// The winning block's pilot ids are the per-member renown ledger key (WinnerUserIDs -> CreditMemberContributions).
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
	// WinnerUserIDs are the "US.." ids of the winning squad's pilots who actually fought this battle, read
	// from that block's pilot array. The report assembler (sub_8217D4D0) writes up to reportUsersPerBlock
	// pilots per block; this drives the per-member renown ledger (CreditMemberContributions).
	WinnerUserIDs []string
}

// Per-block pilot array (report assembler sub_8217D4D0): up to 6 user ids, 19 bytes each, at block+0x25 --
// i.e. body 0x4A for block-1 (base 0x25) and 0x151 for block-2 (base 0x12C). Non-empty slots = who fought.
const (
	reportUsersPerBlock = 6
	reportUserIDBytes   = 19
	reportBlock1Users   = 0x4A
	reportBlock2Users   = 0x151
)

// blockUserIDs reads a block's up-to-6 pilot user ids (19-byte slots) and returns the non-empty ones.
func blockUserIDs(b []byte, base int) []string {
	ids := make([]string, 0, reportUsersPerBlock)
	for i := 0; i < reportUsersPerBlock; i++ {
		start := base + i*reportUserIDBytes
		if start+reportUserIDBytes > len(b) {
			break
		}
		if id := trimNullString(b[start : start+reportUserIDBytes]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
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
		r.WinnerUserIDs = blockUserIDs(b, reportBlock1Users)
	case nationB:
		r.WinnerTeam, r.LoserTeam, r.LoserNation = teamB, teamA, nationA
		r.WinnerMerit = int32(b[0x223]) // team-1 merit slot
		r.WinnerUserIDs = blockUserIDs(b, reportBlock2Users)
	default:
		return r, false
	}
	return r, true
}

type battleReportServer struct {
	*messageServer
	// applier holds all world/squad logic (captures, locks, dissolution/revival, events). ingest just
	// parses the packet and hands the result off, so the CLI simulator and tests drive the same code.
	applier *BattleApplier
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

	s.applier.Apply(ctx, res)
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
// A capital falling AWAY FROM ITS DEFAULT NATION is a dissolution; a capital held by a non-default
// nation changing hands is an ordinary region capture (see hqDefaultNation).
func isHQ(areaID byte) bool { return areaID == 1 || areaID == 2 || areaID == 3 }

// hqDefaultNation maps a capital area to the nation that owns it by default (area 1 Xeres->Tarakia,
// 2 Ostrov->Morskoj, 3 Qara->Sal Kar); 0 for a non-capital area.
func hqDefaultNation(areaID byte) byte {
	switch areaID {
	case 1:
		return 'A'
	case 2:
		return 'B'
	case 3:
		return 'C'
	}
	return 0
}

// revivalRow maps a revived nation to its "Guerrilla Recaptures <capital>" WORLD_NEWS param row
// (WorldSituationInfoNewsParam rows 7-9; the nation is baked into the text, like the dissolution rows):
// A -> 7 (Xeres/Tarakia), B -> 8 (Ostrov/Morskoj), C -> 9 (Qara/Sal Kar).
func revivalRow(nation byte) (int32, bool) {
	switch nation {
	case 'A':
		return 7, true
	case 'B':
		return 8, true
	case 'C':
		return 9, true
	}
	return 0, false
}

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
	// Note: HQ *dissolutions* are filtered by the caller (recordHQFall), so an HQ area reaching here is
	// an ordinary capture between non-default holders and DOES get a region story.
	if beforeOwner == 0 || afterOwner == 0 || beforeOwner == afterOwner {
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

func NewBattleReportServer(listenAddress net.IP, serverConfig config.EventServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, squadRepo *SquadRepository, worldRepo *WorldRepository) *battleReportServer {
	s := &battleReportServer{applier: NewBattleApplier(worldRepo, squadRepo, serverConfig.GenerateEvents, serverConfig.CpuBattleScale, serverConfig.Label)}

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
