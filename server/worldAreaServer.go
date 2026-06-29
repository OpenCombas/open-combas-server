package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"context"
	"net"
	"sync"
	"time"

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

func CreateAreaState(xuid [16]byte, order [8]byte) AreaState {
	var seasonID [19]byte
	copy(seasonID[:], "0001")

	world := WorldHeader{
		Status:   0,
		SeasonID: seasonID,
		DataTime: int32(time.Now().Unix()),
	}

	// The world has 22 areas (observed in capture: the client only ever requests area ids 1..22).
	// The wire format is fixed at 25 records (the parser sub_823BD498 always reads 25); AreaNum
	// tells the client how many are valid, so we populate the first 22 and leave the rest zeroed.
	// Control is derived from the shared static world model (worldData.go) so the war-map owner flag
	// matches the per-area battlefield occupation served by the area-info (197) server. AreaID is
	// 1-based: the war-map nodes are static client-side, but the per-area info request keys off
	// this id, and a 0-based id resolved to the wrong (north) neighbour in testing.
	const areaCount = 22
	var areas [25]AreaRecord
	for i := 0; i < areaCount; i++ {
		areaID := byte(i + 1)
		owner, pointsA, pointsB, pointsC := areaControlSummary(areaID)
		a := AreaRecord{
			AreaID:        areaID,
			OwningFaction: owner,                // sets the map owner flag (was unset -> only Morskoj rendered)
			AreaPoints:    battlefieldCapacity,  // per-nation occupation denominator (matches the points' 0..cap scale)
			PointsA:       pointsA,              // averaged per-nation occupation (0..capacity), summing to capacity
			PointsB:       pointsB,
			PointsC:       pointsC,
		}
		switch owner { // priority-target marker for the owning nation
		case 'A':
			a.PriorityA = 1
		case 'B':
			a.PriorityB = 1
		case 'C':
			a.PriorityC = 1
		}
		areas[i] = a
	}

	return AreaState{
		Header:  CreateHeader(xuid, order),
		World:   world,
		AreaNum: areaCount,
		Areas:   areas,
	}
}

type worldAreaServer struct {
	*messageServer
}

func NewWorldAreaServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *worldAreaServer {
	s := &worldAreaServer{}

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
			return CreateAreaState(hi.Xuid, hi.Order)
		},
		responseSize: constants.WorldAreaResponseSize,
	}

	return s
}
