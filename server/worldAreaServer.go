package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// World Area / map-control response.
//
// Reverse-engineered from Release.xex parser sub_823BD498 (internal message code 196). The World
// manager issues nation (195) and area (196) queries on the SAME connection; the client adds the
// message code to the destination port (sub_823B6AB8: LOWORD(destAddr) += code). With nation on
// 1215 (base 1020 + 195), the area query lands on port 1216 (base 1020 + 196). Little-endian,
// C-struct aligned. See SERVER_ANALYSIS.md §6.

// AreaRecord is one map-area control record (28 bytes). Wire format string "C3,I,C,I3,C3".
// Field semantics confirmed from the parser sub_823BD498 debug labels (Shift-JIS):
type AreaRecord struct {
	AreaID        byte    // off 0  - エリアID
	OwningFaction byte    // off 1  - エリア所有勢力 (controlling nation as char 'A'/'B'/'C'; drives the map owner flag)
	HQFlag        byte    // off 2  - 本拠地フラグ (area is a nation headquarters)
	_             byte    // align
	AreaPoints    int32   // off 4  - エリア占領ポイント (total area occupation points)
	BattleFlag    byte    // off 8  - 激戦エリアフラグ (fierce-battle flag)
	_             [3]byte // align
	PointsA       int32   // off 12 - A国占領ポイント (nation A occupation points)
	PointsB       int32   // off 16 - B国占領ポイント (nation B occupation points)
	PointsC       int32   // off 20 - C国占領ポイント (nation C occupation points)
	PriorityA     byte    // off 24 - A国重点目標エリアフラグ (nation A priority-target flag; drawn for own nation)
	PriorityB     byte    // off 25 - B国重点目標エリアフラグ (nation B priority-target flag)
	PriorityC     byte    // off 26 - C国重点目標エリアフラグ (nation C priority-target flag)
	_             byte    // pad to 28
}

// AreaState body is 732 bytes: WorldHeader(28) + AreaNum(+pad)(4) + 25*AreaRecord(700).
// Total encoded size = constants.WorldAreaResponseSize (764, incl. 32-byte MessageHeader).
type AreaState struct {
	Header  MessageHeader
	World   WorldHeader
	AreaNum byte
	_       [3]byte
	Areas   [25]AreaRecord
}

// worldAreaCount is the number of real areas (the client only ever requests ids 1..22). The wire format
// is fixed at 25 records (parser sub_823BD498 always reads 25); AreaNum tells the client how many are
// valid, so we populate the first 22 and leave the rest zeroed.
const worldAreaCount = 22

// newAreaState assembles the war-map reply, taking per-area control from `summary`. AreaID is 1-based:
// the war-map nodes are static client-side, but the per-area info request keys off this id, and a
// 0-based id resolved to the wrong (north) neighbour in testing.
func newAreaState(xuid [16]byte, order [8]byte, summary func(areaID byte) (owner byte, pointsA, pointsB, pointsC int32), fierce func(areaID byte) bool) AreaState {
	var areas [25]AreaRecord
	for i := 0; i < worldAreaCount; i++ {
		areaID := byte(i + 1)
		owner, pointsA, pointsB, pointsC := summary(areaID)
		var battleFlag byte
		if fierce != nil && fierce(areaID) {
			battleFlag = 1 // 激戦エリアフラグ: >30% of this area's battle reports have been PvP
		}
		// NOTE: PriorityA/B/C (重点目標エリアフラグ) are deliberately left 0. They draw the orange
		// "priority strategic target" ring on the war map for the player's own nation and are meant to be
		// set only by a war event, never as part of the default/reset state. Setting them per owning
		// nation put a ring on every Tarakian area. An event system will drive these later.
		areas[i] = AreaRecord{
			AreaID:        areaID,
			OwningFaction: owner,               // sets the map owner flag (was unset -> only Morskoj rendered)
			AreaPoints:    battlefieldCapacity, // per-nation occupation denominator (matches the points' 0..cap scale)
			BattleFlag:    battleFlag,          // 激戦エリアフラグ (fierce-battle flag; PvP-driven)
			PointsA:       pointsA,             // averaged per-nation occupation (0..capacity), summing to capacity
			PointsB:       pointsB,
			PointsC:       pointsC,
		}
	}

	return AreaState{
		Header:  CreateHeader(xuid, order),
		World:   worldHeaderNow(),
		AreaNum: worldAreaCount,
		Areas:   areas,
	}
}

func CreateAreaState(xuid [16]byte, order [8]byte) AreaState {
	// Static model: control derived from worldData.go so the war-map owner flag matches the per-area
	// battlefield occupation served by the area-info (197) server. No fierce-battle flags without live data.
	return newAreaState(xuid, order, areaControlSummary, nil)
}

type worldAreaServer struct {
	*messageServer
	repo *WorldRepository // nil when Mongo is disabled -> static model
}

// buildArea serves the war map from Mongo when wired, deriving each area's control from its stored
// battlefields; any read error falls back to the static model.
func (s *worldAreaServer) buildArea(hi UserHelloMessage) AreaState {
	if s.repo != nil {
		readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
		defer cancel()
		if grouped, err := s.repo.BattlefieldsGrouped(readCtx); err != nil {
			logging.Warn.Printf("[%s] mongo read failed, using static model: %v", s.serverConfig.Label, err)
		} else if len(grouped) > 0 {
			// Fierce-battle flags are best-effort: a read error just leaves them unset.
			fierce, ferr := s.repo.AreaFierceFlags(readCtx)
			if ferr != nil {
				logging.Warn.Printf("[%s] fierce-flag read failed, leaving unset: %v", s.serverConfig.Label, ferr)
			}
			return newAreaState(hi.Xuid, hi.Order, func(areaID byte) (byte, int32, int32, int32) {
				return areaSummaryFrom(grouped[areaID])
			}, func(areaID byte) bool {
				return fierce[int32(areaID)]
			})
		}
	}
	return CreateAreaState(hi.Xuid, hi.Order)
}

func NewWorldAreaServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldAreaServer {
	s := &worldAreaServer{repo: repo}

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
			return validateWorldPacket(packet, clientAddr, serverConfig.Label) // same CH + size checks
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			return s.buildArea(hi)
		},
		responseSize: constants.WorldAreaResponseSize,

		// Taint the 3 UNUSED area records (indices 22..24; only 1..22 are real). Body =
		// WorldHeader(28) + AreaNum(+pad)(4) + 25*AreaRecord(28); record 22 starts at body 32+22*28 = 648,
		// so buffer offset = MessageHeader(32) + 648 = 680, length 3*28 = 84. See taint.go.
		taintTag:   TaintWorldArea,
		taintStart: constants.MinHelloMessageSize + 32 + 22*28, // 680
		taintLen:   3 * 28,                                     // 84
	}

	return s
}
