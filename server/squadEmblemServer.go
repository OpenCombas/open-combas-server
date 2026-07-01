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

// Squad emblem upload (port 1020+msgCode 186 = 1206 debug; retail 1246). Builder sub_823BF0E0 sends a
// 228-byte body: gamertag(16) + teamid(18) + 2 reserved + 16 emblem layers (12 bytes each, wire schema
// "S4,C3") starting at offset 36; recordSize -1; parser sub_823BDA78 reads body[0]: '1' "Emblem Config
// Complete", anything else "Unknown Error". The 16-layer blob is exactly the emblem array the squad-login
// (1204) response already carries, so we store it raw and hand it back on login. With no repository we
// degrade to a plain '1' ack. See project_combas_server_protocol.

const (
	squadEmblemBodySize = 228 // gamertag(16)+teamid(18)+2 reserved + 16*12 emblems
	squadEmblemOffset   = 36  // emblem array start within the body
	squadEmblemBlobSize = 192 // 16 layers * 12 bytes
)

// parseSquadEmblem extracts the team id and the raw 192-byte emblem blob.
func parseSquadEmblem(packet []byte) (teamID string, emblems []byte, ok bool) {
	if len(packet) < constants.MinHelloMessageSize+squadEmblemBodySize {
		return "", nil, false
	}
	body := packet[constants.MinHelloMessageSize:]
	teamID = trimNullString(body[16:34])
	emblems = append([]byte(nil), body[squadEmblemOffset:squadEmblemOffset+squadEmblemBlobSize]...)
	return teamID, emblems, true
}

type squadEmblemServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> plain '1' ack
}

func (s *squadEmblemServer) buildEmblem(hi UserHelloMessage, packet []byte) SquadAckState {
	if s.repo == nil {
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	teamID, emblems, ok := parseSquadEmblem(packet)
	if !ok || teamID == "" {
		logging.Warn.Printf("[%s] could not parse emblem upload, acking", s.serverConfig.Label)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	found, err := s.repo.SetEmblems(readCtx, teamID, emblems)
	if err != nil {
		logging.Warn.Printf("[%s] emblem persist failed, acking: %v", s.serverConfig.Label, err)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}
	if !found {
		return squadAckState(hi.Xuid, hi.Order, '2') // -> "Unknown Error" (no team)
	}
	logging.Info.Printf("[%s] stored emblem for %s", s.serverConfig.Label, teamID)
	return squadAckState(hi.Xuid, hi.Order, '1') // Emblem Config Complete
}

func NewSquadEmblemServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadEmblemServer {
	s := &squadEmblemServer{repo: repo}

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
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			resp := s.buildEmblem(hi, *readBuffer)
			buf := make([]byte, constants.SquadAckResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
