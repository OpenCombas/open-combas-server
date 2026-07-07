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

// Squad member "hound number" assignment (port 1020+msgCode 204 = 1224 debug; retail 1264). Builder
// sub_823BF050 (via sub_823BFE08/sub_823B8C40) sends a fixed 57-byte body; recordSize -1; parser
// sub_823BDA28 reads body[0]: '1' "Member Config Complete", '2' "Already Been Used By Other Users",
// '3' "Target Member Not Exist". An unanswered request shows "communication failed".
//
// Body layout (offsets within the 57-byte body, from the packers): gamertag[0..15], teamid[16..33],
// userid[35..52] (null-separated), and the NEW NUMBER at byte [55] (sub_823B8C40 sets v12 = number,
// clamped to <=100). We assign it to the member when free, '2' if another member holds it. With no
// repository we degrade to a plain '1' ack. See project_combas_server_protocol.

const squadMemberNumberBodySize = 57

func trimNullString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// parseSquadMemberNumber extracts gamertag, team id, user id and the new number from the 57-byte body.
func parseSquadMemberNumber(packet []byte) (gamertag, teamID, userID string, number byte, ok bool) {
	if len(packet) < constants.MinHelloMessageSize+squadMemberNumberBodySize {
		return "", "", "", 0, false
	}
	body := packet[constants.MinHelloMessageSize:]
	gamertag = trimNullString(body[0:16])
	teamID = trimNullString(body[16:34])
	userID = trimNullString(body[35:54])
	number = body[55]
	return gamertag, teamID, userID, number, true
}

type squadMemberNumberServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> plain '1' ack
}

func (s *squadMemberNumberServer) buildMemberNumber(hi UserHelloMessage, packet []byte) SquadAckState {
	if s.repo == nil {
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	_, teamID, userID, number, ok := parseSquadMemberNumber(packet)
	if !ok || teamID == "" {
		logging.Warn.Printf("[%s] could not parse member-number request, acking", s.serverConfig.Label)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	// Resolve the acting member by the reliable header XUID first; the body User ID can be the mis-adopted
	// leader US (see SetMemberNumber), which would set the wrong member's hound number.
	status, err := s.repo.SetMemberNumber(readCtx, teamID, string(hi.Xuid[:]), userID, number)
	if err != nil {
		logging.Warn.Printf("[%s] set member number failed, acking: %v", s.serverConfig.Label, err)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}
	logging.Info.Printf("[%s] set number %d for xuid %s (body userID %q) in %s -> status %q", s.serverConfig.Label, number, string(hi.Xuid[:]), userID, teamID, status)
	return squadAckState(hi.Xuid, hi.Order, status)
}

func NewSquadMemberNumberServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadMemberNumberServer {
	s := &squadMemberNumberServer{repo: repo}

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
			resp := s.buildMemberNumber(hi, *readBuffer)
			buf := make([]byte, constants.SquadAckResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
