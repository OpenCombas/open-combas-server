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

// World nation donation (port 1020+msgCode 206 = 1226 debug; retail 1266). Builder sub_823BF8D0 sends
// body "<gamertag>,<nation>" (printf "%s,%c") from the World-screen "donate to nation" action
// (caller sub_821DB5E8: gamertag via XamUserGetName, nation char from an "AABC" affiliation table).
// The response is a 1-byte status record read by parser sub_823BDFB0:
//   '1' "Complete"        - donation accepted
//   '2' "Country is Dead" - target nation eliminated
//   '3' "Acceptance end"  - donation window closed
//   other                 - "Unknown Error"
//
// The request carries NO amount -- only gamertag + nation -- because the donation amount is HARD-SET
// client-side to a fixed $10,000,000 per donation (operator-confirmed 2026-07-09). Step 1.5: this handler
// applies that fixed credit to the target nation's totalIncome ("Total Revenue") via
// WorldRepository.CreditNationDonation, which is what surfaces on the World Situation faction screen.
// Deferred step 2 (RE-gated) is any FURTHER world-model effect + the dedicated donation section of the
// World Situation tail (worldServer.go WorldState.Tail).
// An unanswered 1266 makes the world-screen donation retry every ~30s and fail with "communication failed".

// parseDonation extracts the gamertag and target nation byte from a "<gamertag>,<nation>" body.
func parseDonation(packet []byte) (gamertag string, nation byte, ok bool) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", 0, false
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) < 2 || len(strings.TrimSpace(parts[1])) == 0 {
		return "", 0, false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])[0], true
}

type worldDonationServer struct {
	*messageServer
	repo *WorldRepository // nil when Mongo is disabled -> always acknowledges '1'
}

// donationAmount is the fixed sum a single world-screen donation adds to the target nation's Total
// Revenue. The client hard-sets every donation to $10,000,000 and does not carry it in the request.
const donationAmount int32 = 10_000_000

// buildDonation credits the donation to the target nation's Total Revenue (step 1.5) and returns the
// status the client expects: '1' Complete, '2' Country is Dead, '3' Acceptance end (unknown nation).
// With no world repository wired it degrades to a plain '1' ack so the world screen still unblocks.
func (s *worldDonationServer) buildDonation(hi UserHelloMessage, packet []byte) SquadAckState {
	gamertag, nation, ok := parseDonation(packet)
	if !ok {
		// Lenient: acknowledge so the world screen never wedges on a body we couldn't parse.
		logging.Warn.Printf("[%s] could not parse donation, acking Complete", s.serverConfig.Label)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	status := byte('1') // Complete (static fallback when no world repo is wired)
	if s.repo != nil {
		writeCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
		defer cancel()
		if st, err := s.repo.CreditNationDonation(writeCtx, nation, donationAmount); err != nil {
			// Prioritise not wedging the client: ack Complete even though the credit didn't persist.
			logging.Warn.Printf("[%s] donation credit failed, acking Complete: %v", s.serverConfig.Label, err)
		} else {
			status = st
		}
	}
	logging.Info.Printf("[%s] donation from %s to nation %q (+%d) -> status %q", s.serverConfig.Label, gamertag, string(nation), donationAmount, string(status))
	return squadAckState(hi.Xuid, hi.Order, status)
}

func NewWorldDonationServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldDonationServer {
	s := &worldDonationServer{repo: repo}

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
			return validateWorldPacket(packet, clientAddr, serverConfig.Label) // CH magic + min-size
		},
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			resp := s.buildDonation(hi, *readBuffer)
			buf := make([]byte, constants.SquadAckResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
