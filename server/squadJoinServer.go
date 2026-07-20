package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Squad join-commit (port 1020+msgCode 182 = 1202 debug; retail 1242). Sent by the squad HOST after the
// P2P join handshake reaches 0x4004 (begin-join sub_823B8A38 -> sub_823C0500 -> builder sub_823BF1A0).
// It registers the joining player with the backend and expects an assigned User ID back; with no server
// bound the title's join state machine times out and "unable to connect" retries follow.
//
// Request body (ASCII, comma-joined, "%s,%s,%lld,%s,%d,%d"):
//   gamertag , teamID , joinerXUID(int64 decimal) , name , rank , n
// e.g. "ibacgns,TM0001000000000001,2533275239575185,ibac,34,0"
// NOTE: the packet HEADER XUID is the HOST's (the sender); the JOINER's XUID is the %lld body field
// (decimal), which we convert to the 16-char upper-hex form used as the profile/member key.
//
// Response (recordSize 40; parser sub_823BDAA0 reads body[0] status, body[2..20) User ID):
//   '1' success (+ 18-char User ID)   '2' "Member Number Over Error" (squad full)
//   '3' "User Name Unique Error"      '4' "Hound Number Unique Error"   other -> "Unknown Error"

// SquadJoinRecord is the 40-byte join response body. Mirrors the reg record but carries only the User ID.
type SquadJoinRecord struct {
	Status byte     // off 0  - '1' success / '2' full / '3' name-unique / '4' hound-unique / other unknown
	Sep    byte     // off 1  - separator (skipped by parser)
	UserID [18]byte // off 2  - "US" + 16 digits (assigned user id), read by sub_823BDAA0
	_      [20]byte // off 20 - pad to 40
}

// SquadJoinState is the full reply: header(32) + record(40) = constants.SquadJoinResponseSize (72).
type SquadJoinState struct {
	Header MessageHeader
	Record SquadJoinRecord
}

func squadJoinState(xuid [16]byte, order [8]byte, status byte, userID string) SquadJoinState {
	rec := SquadJoinRecord{Status: status, Sep: ','}
	copy(rec.UserID[:], userID)
	return SquadJoinState{Header: CreateHeader(xuid, order), Record: rec}
}

// parseSquadJoin extracts the join fields from the comma-joined body. The joiner XUID is returned as the
// 16-char upper-hex string (converted from the decimal %lld field) so it keys profiles the same way the
// joiner's own message headers do.
func parseSquadJoin(packet []byte) (gamertag, teamID, joinerXUIDHex, name string, rank int32, ok bool) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", "", "", "", 0, false
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) < 4 {
		return "", "", "", "", 0, false
	}
	gamertag = strings.TrimSpace(parts[0])
	teamID = strings.TrimSpace(parts[1])
	if v, err := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64); err == nil {
		joinerXUIDHex = fmt.Sprintf("%016X", v)
	}
	name = strings.TrimSpace(parts[3])
	if len(parts) >= 5 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[4])); err == nil {
			rank = int32(v)
		}
	}
	ok = teamID != "" && joinerXUIDHex != ""
	return
}

type squadJoinServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> static fixed-id success
}

// CreateSquadJoinState is the static fallback (Mongo disabled): always succeeds with a fixed User ID so
// the join never hard-fails during bring-up.
func CreateSquadJoinState(xuid [16]byte, order [8]byte) SquadJoinState {
	return squadJoinState(xuid, order, '1', "US0001000000000001")
}

func (s *squadJoinServer) buildJoin(hi UserHelloMessage, packet []byte) SquadJoinState {
	if s.repo == nil {
		return CreateSquadJoinState(hi.Xuid, hi.Order)
	}

	gamertag, teamID, joinerXUID, name, rank, ok := parseSquadJoin(packet)
	if !ok {
		logging.Warn.Printf("[%s] could not parse join request, using static join", s.serverConfig.Label)
		return CreateSquadJoinState(hi.Xuid, hi.Order)
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	status, userID, err := s.repo.AddMember(readCtx, teamID, joinerXUID, gamertag, name, rank)
	if err != nil {
		logging.Warn.Printf("[%s] add member failed, using static join: %v", s.serverConfig.Label, err)
		return CreateSquadJoinState(hi.Xuid, hi.Order)
	}

	// ⚠ TEMPORARY DIAGNOSTIC -- FIX #1 DELIBERATELY RE-APPLIED (reinstates the pre-8ff25d4 behaviour).
	//
	// Echo the REQUESTER's (packet-header XUID = the HOST's) OWN User ID instead of the joiner's freshly
	// minted one. Consequence chain, which is the POINT of this diagnostic: the host copies the 182 reply US
	// into its own myIdentity (+2668) -- here a no-op, it re-writes its own id -- and then uses that same
	// +2668 as the source for the joiner's US slot in the P2P 0x400B it sends the joiner (US@+0x3C). So the
	// joiner adopts the HOST's US and BOTH CONSOLES END UP SHARING ONE US.
	//
	// We are reproducing that state on purpose: it is the configuration in which parallel lobbies were
	// reported eliminated (operator recollection, corroborated by 32d7cad's RemoveMember comment -- "a joiner
	// commonly carries the LEADER's US in its own myIdentity", confirmed 2026-07-07 with both consoles'
	// saves storing US0001000000000001). We want the wire flow of a converging lobby merge to compare
	// against the current failure.
	//
	// KNOWN COST, accepted for the duration of this experiment: with the joiner carrying the leader's US,
	// a 183 withdraw body points at the LEADER, which wedges on the leader-with-members guard -- i.e. this
	// re-breaks leaving a squad. RemoveMember resolves by header XUID first (32d7cad) which mitigates it,
	// but do not treat withdraw as trustworthy while this is applied. REVERT once the successful-join shape
	// is understood; see 8ff25d4 for the revert this undoes.
	replyUserID := userID
	if status == '1' {
		if p, perr := s.repo.ProfileByXUID(readCtx, string(hi.Xuid[:])); perr != nil {
			logging.Warn.Printf("[%s] join: could not resolve requester %s profile, echoing joiner US: %v", s.serverConfig.Label, string(hi.Xuid[:]), perr)
		} else if p != nil && p.UserID != "" {
			replyUserID = p.UserID
		}
	}
	logging.Warn.Printf("[%s] FIX#1-DIAGNOSTIC join %s (joiner %s) -> squad %s: status %q joinerUserID %q replyUserID(requester/HOST) %q -- joiner will adopt the host US",
		s.serverConfig.Label, gamertag, joinerXUID, teamID, status, userID, replyUserID)
	return squadJoinState(hi.Xuid, hi.Order, status, replyUserID)
}

func NewSquadJoinServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadJoinServer {
	s := &squadJoinServer{repo: repo}

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
			resp := s.buildJoin(hi, *readBuffer)
			buf := make([]byte, constants.SquadJoinResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
