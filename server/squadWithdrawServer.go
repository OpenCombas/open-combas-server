package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Squad withdraw / leave (port 1020+msgCode 183 = 1203 debug; retail base 1060 -> 1243). Builder
// sub_823BF2B8 sends "<gamertag>,<teamid>,<userid>"; recordSize 2; parser sub_823BDB88 reads body[0]:
//   '1' "Delete Complete" (left), '2' "Leader Can't Delete", '3' "Target Data None", '4' "Server
//   Resetting". An unanswered 1243 makes the leave action retry and fail.
//
// We remove the member from the squad doc and unlink their profile; a solo leader leaving disbands the
// squad. With no repository (Mongo off) we degrade to a plain '1' ack. See project_combas_server_protocol.

// parseSquadWithdraw extracts gamertag, team id and user id from a "<gamertag>,<teamid>,<userid>" body.
func parseSquadWithdraw(packet []byte) (gamertag, teamID, userID string) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", "", ""
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) >= 3 {
		gamertag = strings.TrimSpace(parts[0])
		teamID = strings.TrimSpace(parts[1])
		userID = strings.TrimSpace(parts[2])
	}
	return
}

type squadWithdrawServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> plain '1' ack
}

func (s *squadWithdrawServer) buildWithdraw(hi UserHelloMessage, packet []byte) SquadAckState {
	if s.repo == nil {
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	_, teamID, userID := parseSquadWithdraw(packet)
	if teamID == "" {
		logging.Warn.Printf("[%s] could not parse withdraw, acking", s.serverConfig.Label)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	// Resolve the leaver by the reliable header XUID first; the body User ID can be the LEADER's US when
	// a joiner has adopted the leader's identity in-game (see RemoveMember), which would otherwise remove
	// the wrong member or wedge on the leader-with-members guard.
	status, err := s.repo.RemoveMember(readCtx, teamID, string(hi.Xuid[:]), userID)
	if err != nil {
		logging.Warn.Printf("[%s] withdraw failed, acking: %v", s.serverConfig.Label, err)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}
	logging.Info.Printf("[%s] withdraw xuid %s (body userID %q) from %s -> status %q", s.serverConfig.Label, string(hi.Xuid[:]), userID, teamID, status)
	return squadAckState(hi.Xuid, hi.Order, status)
}

func NewSquadWithdrawServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadWithdrawServer {
	s := &squadWithdrawServer{repo: repo}

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
			resp := s.buildWithdraw(hi, *readBuffer)
			buf := make([]byte, constants.SquadAckResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
