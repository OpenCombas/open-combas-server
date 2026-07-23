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

// Map Detail (per-map / battle detail) response.
//
// Reverse-engineered from Release.xex parser sub_823BD7B0 (internal message code 198). Reached on
// port 1218 (World base 1020 + 198; retail 1258). Request body is "<gamertag>,<areaID>,<mapID>,<sub>"
// (builder sub_823BED88, format "%s,%d,%d,%d"). Response body is 60 bytes, little-endian, C-aligned:
// WorldHeader(28) + a single AreaMapRecord(32) -- the detail view for ONE battlefield, sharing the
// area-info (code 197) record layout. This is what the mission-load screen reads, so it must match the
// area-info view: served from Mongo (the reset/war state) when wired, else the static model.

// MapDetailState body is 60 bytes: WorldHeader(28) + one AreaMapRecord(32).
// Total encoded size = constants.MapDetailResponseSize (92, incl. 32-byte MessageHeader).
type MapDetailState struct {
	Header MessageHeader
	World  WorldHeader
	Map    AreaMapRecord
}

func mapDetailState(xuid [16]byte, order [8]byte, rec AreaMapRecord) MapDetailState {
	return MapDetailState{
		Header: CreateHeader(xuid, order),
		World:  worldHeaderNow(),
		Map:    rec,
	}
}

// parseMapDetailIDs extracts (areaID, mapID) from a body "<gamertag>,<areaID>,<mapID>,<sub>"
// (e.g. "ibac,20,2,0" = Old Berri Coal Mine). The trailing sub-id isn't needed to key the battlefield.
func parseMapDetailIDs(packet []byte) (areaID, mapID byte) {
	if len(packet) <= constants.MinHelloMessageSize {
		return 0, 0
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	atob := func(s string) byte {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return byte(n)
		}
		return 0
	}
	if len(parts) >= 3 {
		return atob(parts[1]), atob(parts[2])
	}
	return 0, 0
}

type worldMapDetailServer struct {
	*messageServer
	repo *WorldRepository // nil when Mongo is disabled -> static model
}

// mapRecord resolves one battlefield's AreaMapRecord and applies the between-seasons lock: while the season
// has not started, the map is locked for deployment (matching the area-info view).
func (s *worldMapDetailServer) mapRecord(areaID, mapID byte) AreaMapRecord {
	rec := s.resolveMapRecord(areaID, mapID)
	if SeasonLocked() {
		rec.MapLockFlag = 1
	}
	return rec
}

// resolveMapRecord fetches the record from Mongo when wired (so it matches the area-info view and the reset
// war state), falling back to the static model on miss/error.
func (s *worldMapDetailServer) resolveMapRecord(areaID, mapID byte) AreaMapRecord {
	if s.repo != nil {
		readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
		defer cancel()
		if bf, err := s.repo.BattlefieldByAreaMap(readCtx, areaID, mapID); err != nil {
			logging.Warn.Printf("[%s] mongo read failed, using static model: %v", s.serverConfig.Label, err)
		} else if bf != nil {
			return bf.toAreaMapRecord()
		}
	}
	// Static fallback: the area's battlefields from worldData.go.
	if maps, count := areaBattlefields(areaID); mapID >= 1 && mapID <= count {
		return maps[mapID-1]
	}
	return AreaMapRecord{MapID: int16(mapID)}
}

func NewWorldMapDetailServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldMapDetailServer {
	s := &worldMapDetailServer{repo: repo}

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
		// buildResponse (not buildPayload) so we can read the selected area/map ids from the body.
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			areaID, mapID := parseMapDetailIDs(*readBuffer)
			resp := mapDetailState(hi.Xuid, hi.Order, s.mapRecord(areaID, mapID))
			buf := make([]byte, constants.MapDetailResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
