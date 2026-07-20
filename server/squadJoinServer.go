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

	// Return the JOINER's freshly-assigned User ID -- the title contract. The host copies the 182 reply US
	// into its own myIdentity (+2668) AND uses that same +2668 as the source for the joiner's US slot in the
	// P2P 0x400B squad-data packet it sends the joiner (US@+0x3C), so the joiner adopts whatever US we return
	// here. Returning the joiner's US therefore delivers the joiner its OWN US end-to-end.
	//
	// [Fix #1 reverted 2026-07-08.] Fix #1 echoed the REQUESTER's (host's) US here to avoid the 182 reply
	// "clobbering" the host's myIdentity, but the RE session traced +2668's lifetime and showed that clobber
	// is HARMLESS: the host's leader/lobby identity reads profile.US / SquadState+56 (set at login), and the
	// +2668->profile mirror (sub_8234C8C8) only fires at reg/login (mgr[189]==3), never during a join
	// (mgr[189]==5). So fix #1 fixed nothing on the host and instead made the host emit US...0001 in 0x400B,
	// causing the joiner to adopt the leader's US. "Host can't disband" has a separate root (suspect the
	// login roster UserName=rec.Name). See xex_re.md "INTERPRETATION".
	//
	// [Fix #1 RE-TESTED 2026-07-19 -- NEGATIVE, do not try again.] Temporarily re-applied to reproduce the
	// "shared US" state (joiner adopts the host US) on a recollection that parallel lobbies were once absent
	// in that configuration. They were not: the lobby merge failed identically.
	//
	// [CORRECTION 2026-07-20.] An earlier revision of this comment claimed no fully successful join existed in
	// any capture. That was wrong -- it came from summarising a two-join capture with `tail`, which showed only
	// the second (failed) join. The first held for 80s. That pair is now the primary evidence for the join
	// timing model. See workspace squad_join_consolidated.md.

	// The joiner is about to tear down its applicant session and reconnect as a squad member, racing the
	// host's stale-entry retirement. Flag them so their imminent 184 login reply is held long enough for the
	// host to retire the old entry first; see buildLogin in squadLoginServer.go for the full derivation.
	pendingMemberReconnects.Mark(joinerXUID)

	logging.Info.Printf("[%s] join %s (joiner %s) -> squad %s: status %q userID %q", s.serverConfig.Label, gamertag, joinerXUID, teamID, status, userID)
	return squadJoinState(hi.Xuid, hi.Order, status, userID)
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
