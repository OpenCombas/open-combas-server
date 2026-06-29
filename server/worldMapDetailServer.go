package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Map Detail (per-map / battle detail) response.
//
// Reverse-engineered from Release.xex parser sub_823BD7B0 (internal message code 198). Reached on
// port 1218 (World base 1020 + 198). Request body is "<gamertag>,<a>,<b>,<c>" (builder
// sub_823BED88, format "%s,%d,%d,%d"): the player's gamertag followed by three ids identifying the
// selected map (area id / map id / sub-id). Response body is 60 bytes, little-endian, C-aligned:
// WorldHeader(28) + a single AreaMapRecord(32). It is the detail view for ONE map, sharing the same
// "S,C,I6,C" record layout as area-info (code 197). See SERVER_ANALYSIS.md §6.
//
// STUB: field semantics beyond MapID/OccupationPoints are not yet labelled — awaiting in-game test.

// MapDetailState body is 60 bytes: WorldHeader(28) + one AreaMapRecord(32).
// Total encoded size = constants.MapDetailResponseSize (92, incl. 32-byte MessageHeader).
type MapDetailState struct {
	Header MessageHeader
	World  WorldHeader
	Map    AreaMapRecord
}

func CreateMapDetailState(xuid [16]byte, order [8]byte, mapID int16) MapDetailState {
	var seasonID [19]byte
	copy(seasonID[:], "0001")

	world := WorldHeader{
		Status:   0,
		SeasonID: seasonID,
		DataTime: int32(time.Now().Unix()),
	}

	return MapDetailState{
		Header: CreateHeader(xuid, order),
		World:  world,
		Map: AreaMapRecord{
			MapID:            mapID,
			OccupationPoints: 1000,
		},
	}
}

// parseMapID extracts the map identifier from a request body of "<gamertag>,<a>,<b>,<c>".
// We echo the second CSV field (first id after the gamertag) back as the map id so the client
// can match the detail to its selection.
func parseMapID(packet []byte) int16 {
	if len(packet) <= constants.MinHelloMessageSize {
		return 0
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) >= 2 {
		if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			return int16(n)
		}
	}
	return 0
}

type worldMapDetailServer struct {
	*messageServer
}

func NewWorldMapDetailServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *worldMapDetailServer {
	s := &worldMapDetailServer{}

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
		// buildResponse (not buildPayload) so we can read the selected map id from the body.
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			mapID := parseMapID(*readBuffer)
			resp := CreateMapDetailState(hi.Xuid, hi.Order, mapID)
			buf := make([]byte, constants.MapDetailResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
