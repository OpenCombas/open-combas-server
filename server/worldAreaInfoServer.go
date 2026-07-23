package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Area Info (per-area detail) response.
//
// Reverse-engineered from Release.xex parser sub_823BD628 (internal message code 197). Reached on
// port 1217 (World base 1020 + 197; confirmed via Wireshark). Request body is "<gamertag>,<areaID>"
// (the player's gamertag, e.g. "ibac", + the selected area id). Response body is 224 bytes,
// little-endian, C-aligned.
// See SERVER_ANALYSIS.md §6.

// AreaMapRecord is one map/COMBAS within an area (32 bytes). Wire format "S,C,I6,C".
// Field semantics confirmed from the parser sub_823BD628 debug labels (Shift-JIS), cross-checked
// against an in-game debug-log dump for Tajin (area 10).
type AreaMapRecord struct {
	MapID              int16   // off 0  - マップID
	ControllingFaction byte    // off 2  - マップ支配勢力 (owning nation as char 'A'/'B'/'C')
	_                  byte    // align
	Capacity           int32   // off 4  - occupation bar DENOMINATOR (max); server-defined, not logged by parser
	ControlLevel       int32   // off 8  - マップ存久値; occupation bar NUMERATOR = controlling nation's level (<= Capacity)
	OccupationPoints   int32   // off 12 - 占領時ポイント (strategic value -> orange dots)
	InvasionA          int32   // off 16 - A国侵攻度 (Tarakia occupation level; shown in split cell when not leader)
	InvasionB          int32   // off 20 - B国侵攻度 (Morskoj occupation level)
	InvasionC          int32   // off 24 - C国侵攻度 (Sal Kar occupation level)
	MapLockFlag        byte    // off 28 - マップロックフラグ
	_                  [3]byte // pad to 32
}

// AreaInfoState body is 224 bytes: WorldHeader(28) + AreaID/MapCount/BattleFlag(+pad)(4) + 6*Map(192).
// Total encoded size = constants.AreaInfoResponseSize (256, incl. 32-byte MessageHeader).
type AreaInfoState struct {
	Header     MessageHeader
	World      WorldHeader
	AreaID     byte // selected area (echoed from request)
	MapCount   byte // "エリア所属MAP数" - number of valid Maps entries
	BattleFlag byte // "激戦エリアフラグ" - fierce-battle flag
	_          byte
	Maps       [6]AreaMapRecord
}

// newAreaInfoState assembles the per-area battlefield reply from a fixed set of records (static model
// or Mongo-backed).
func newAreaInfoState(xuid [16]byte, order [8]byte, areaID byte, maps [6]AreaMapRecord, mapCount byte, fierce bool) AreaInfoState {
	var battleFlag byte
	if fierce {
		battleFlag = 1 // 激戦エリアフラグ: >30% of this area's battle reports have been PvP
	}
	// Between seasons the war has not started: lock EVERY map for deployment (the allegiance-change window).
	if SeasonLocked() {
		for i := range maps {
			maps[i].MapLockFlag = 1 // マップロックフラグ: closed to missions
		}
	}
	return AreaInfoState{
		Header:     CreateHeader(xuid, order),
		World:      worldHeaderNow(),
		AreaID:     areaID,
		MapCount:   mapCount,
		BattleFlag: battleFlag,
		Maps:       maps,
	}
}

func CreateAreaInfoState(xuid [16]byte, order [8]byte, areaID byte) AreaInfoState {
	// Static model (worldData.go): keeps the count (3 or 4), per-nation occupation and controlling flag
	// consistent with the war-map (area-map / code 196) server.
	maps, mapCount := areaBattlefields(areaID)
	return newAreaInfoState(xuid, order, areaID, maps, mapCount, false)
}

// parseAreaID extracts <id> from a request body of "<gamertag>,<id>" (e.g. "ibac,11").
func parseAreaID(packet []byte) byte {
	if len(packet) <= constants.MinHelloMessageSize {
		return 0
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	if parts := strings.SplitN(string(body), ",", 2); len(parts) == 2 {
		if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			return byte(n)
		}
	}
	return 0
}

type worldAreaInfoServer struct {
	*messageServer
	repo *WorldRepository // nil when Mongo is disabled -> static model
}

// buildAreaInfo serves one area's battlefields from Mongo when wired, falling back to the static model
// on any read error.
func (s *worldAreaInfoServer) buildAreaInfo(hi UserHelloMessage, areaID byte) AreaInfoState {
	if s.repo != nil {
		readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
		defer cancel()
		if bfs, err := s.repo.BattlefieldsByArea(readCtx, areaID); err != nil {
			logging.Warn.Printf("[%s] mongo read failed, using static model: %v", s.serverConfig.Label, err)
		} else if len(bfs) > 0 {
			maps, mapCount := areaMapRecordsFrom(bfs)
			fierce, ferr := s.repo.AreaFierce(readCtx, int32(areaID))
			if ferr != nil {
				logging.Warn.Printf("[%s] fierce-flag read (area %d) failed, leaving unset: %v", s.serverConfig.Label, areaID, ferr)
			}
			return newAreaInfoState(hi.Xuid, hi.Order, areaID, maps, mapCount, fierce)
		}
	}
	return CreateAreaInfoState(hi.Xuid, hi.Order, areaID)
}

func NewWorldAreaInfoServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldAreaInfoServer {
	s := &worldAreaInfoServer{repo: repo}

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
		// buildResponse (not buildPayload) so we can read the requested area id from the body.
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			areaID := parseAreaID(*readBuffer)
			resp := s.buildAreaInfo(hi, areaID)
			buf := make([]byte, constants.AreaInfoResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
