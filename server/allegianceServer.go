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

// Allegiance change (port 1020+msgCode 201 = 1221 debug; retail 1060+201 = 1261). The client offers this at a
// SEASON boundary -- when the war season advances, a squad may switch which nation it fights for. It is a
// different route from deadFlag-driven defection (a squad fleeing an eliminated nation).
//
// WIRE (reverse-engineered from request builder sub_823BF998 + response parser sub_823BE000):
//   - Request  (client -> server): msgCode 201, body printf "%s,%s,%c" = "<account>,<teamId>,<nation>", e.g.
//     "ibacinstall,TM0001000000000032,C" (Sal Kar). Captured as a 65-byte datagram, retried 6x when unanswered.
//   - Response (server -> client): a single status byte read by sub_823BE000 from the first body byte:
//       '1' -> "Complete"                      (allegiance changed)
//       '2' -> "Demand Same As Current State"  (squad already belongs to that nation)
//       other -> "Unknown Error"
//     Served as the standard 34-byte SquadAckState (status at body[0]), same shape as the donation ack.
//
// An unanswered 1221/1261 makes the season-start allegiance prompt fail; the squad's faction is the single
// source of truth for its nation (profiles derive nation from the squad), so the change is one squad update.

// parseAllegiance extracts account, teamId and the target nation byte from a "<account>,<teamId>,<nation>" body.
func parseAllegiance(packet []byte) (account, teamID string, nation byte, ok bool) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", "", 0, false
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) < 3 || len(strings.TrimSpace(parts[2])) == 0 {
		return "", "", 0, false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])[0], true
}

type allegianceServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> always acknowledges '1'
}

// buildAllegiance applies the squad's allegiance change and returns the status the client expects.
// With no squad repository wired it degrades to a plain '1' ack so the season-start prompt still completes.
func (s *allegianceServer) buildAllegiance(hi UserHelloMessage, packet []byte) SquadAckState {
	account, teamID, nation, ok := parseAllegiance(packet)
	if !ok {
		// Lenient: acknowledge so the prompt never wedges on a body we couldn't parse.
		logging.Warn.Printf("[%s] could not parse allegiance request, acking Complete", s.serverConfig.Label)
		return squadAckState(hi.Xuid, hi.Order, allegianceComplete)
	}

	if s.repo == nil {
		logging.Warn.Printf("[%s] no squad repo; acking allegiance Complete for %s -> %q without persisting", s.serverConfig.Label, teamID, string(nation))
		return squadAckState(hi.Xuid, hi.Order, allegianceComplete)
	}

	writeCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	status, err := s.repo.ChangeAllegiance(writeCtx, teamID, nation)
	if err != nil {
		// Datastore fault: prioritise not wedging the client -- ack Complete even though it didn't persist.
		logging.Warn.Printf("[%s] allegiance change failed for %s -> %q, acking Complete: %v", s.serverConfig.Label, teamID, string(nation), err)
		status = allegianceComplete
	}
	logging.Info.Printf("[%s] allegiance: %s squad %s -> nation %q => status %q", s.serverConfig.Label, account, teamID, string(nation), string(status))
	return squadAckState(hi.Xuid, hi.Order, status)
}

func NewAllegianceServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *allegianceServer {
	s := &allegianceServer{repo: repo}

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
			return validateShopPacket(packet, clientAddr, serverConfig.Label) // CH magic + min-size
		},
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			resp := s.buildAllegiance(hi, *readBuffer)
			buf := make([]byte, constants.SquadAckResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
