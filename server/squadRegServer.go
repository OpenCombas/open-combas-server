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

// Squad registration / creation response.
//
// Reverse-engineered from Release.xex: builder sub_823BE928 (internal message code 181) sends
// "<gamertag>,<userid>,<squadname>,<faction 'A'/'B'/'C'>,<n>,<n>" to port 1212+... actually port
// 1020+181 = 1201. recordSize is 44, so the client allocates a 44-byte record buffer -- the old
// static 532-byte buffer overran it (assert "Buffer over run", CH_DataServerCommunicator.cpp:1364).
//
// Response parser sub_823BCCB0 reads, from the 44-byte body:
//   body[0]      = status: '1' success, '2' "Unique Error" (name taken), other "Unknown Error"
//   body[2..19]  = Team ID (18 bytes, "TM" + 16 digits) -> logged "Team ID:%s"
//   body[21..38] = User ID (18 bytes, "US" + 16 digits) -> logged "User ID:%s"
// bytes [1] and [20] are separators the parser skips. Team-id format is "TM%04d%012I64d"
// (season + 12-digit id); all-zero = no team. See SERVER_ANALYSIS.md.

// SquadRegRecord is the 44-byte squad-registration response body.
type SquadRegRecord struct {
	Status byte     // off 0  - '1' success / '2' unique-error / other unknown-error
	Sep1   byte     // off 1  - separator (skipped by parser)
	TeamID [18]byte // off 2  - "TM" + 16 digits (assigned squad id)
	Sep2   byte     // off 20 - separator (skipped by parser)
	UserID [18]byte // off 21 - "US" + 16 digits (assigned user id)
	_      [5]byte  // off 39 - pad to 44
}

// SquadRegState is the full reply: header(32) + record(44). Total = constants.SquadRegResponseSize (76).
type SquadRegState struct {
	Header MessageHeader
	Record SquadRegRecord
}

// squadRegState builds the 44-byte registration reply. status '1' = success, '2' = "Unique Error"
// (name taken), other = "Unknown Error"; the team/user ids are ignored by the client on the error paths.
// The id format must match "TM%04d%012I64d" / "US..." or the login-time team-data validator
// (sub_823BB678) clears it.
func squadRegState(xuid [16]byte, order [8]byte, status byte, teamID, userID string) SquadRegState {
	rec := SquadRegRecord{Status: status, Sep1: ',', Sep2: ','}
	copy(rec.TeamID[:], teamID)
	copy(rec.UserID[:], userID)
	return SquadRegState{Header: CreateHeader(xuid, order), Record: rec}
}

// CreateSquadRegState is the static fallback (Mongo disabled): always succeeds with fixed ids.
func CreateSquadRegState(xuid [16]byte, order [8]byte) SquadRegState {
	return squadRegState(xuid, order, '1', "TM0001000000000001", "US0001000000000001")
}

// parseSquadReg extracts the gamertag, squad name and faction from a registration body of
// "<gamertag>,<userid>,<squadname>,<faction>,<n>,<n>".
func parseSquadReg(packet []byte) (gamertag, name, faction string) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", "", ""
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) >= 4 {
		gamertag = strings.TrimSpace(parts[0])
		name = strings.TrimSpace(parts[2])
		faction = strings.TrimSpace(parts[3])
	}
	return
}

type squadRegServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> static fixed-id reply
}

// buildReg persists the new squad when a repository is wired, returning the assigned ids; on any error
// (or when Mongo is disabled) it falls back to the static fixed-id success reply so creation never fails
// outright.
func (s *squadRegServer) buildReg(hi UserHelloMessage, packet []byte) SquadRegState {
	if s.repo == nil {
		return CreateSquadRegState(hi.Xuid, hi.Order)
	}

	gamertag, name, faction := parseSquadReg(packet)
	if name == "" {
		logging.Warn.Printf("[%s] could not parse squad name, using static reg", s.serverConfig.Label)
		return CreateSquadRegState(hi.Xuid, hi.Order)
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()

	if existing, err := s.repo.SquadByName(readCtx, name); err != nil {
		logging.Warn.Printf("[%s] mongo lookup failed, using static reg: %v", s.serverConfig.Label, err)
		return CreateSquadRegState(hi.Xuid, hi.Order)
	} else if existing != nil {
		return squadRegState(hi.Xuid, hi.Order, '2', "", "") // Unique Error: name already taken
	}

	profile, err := s.repo.EnsureProfile(readCtx, string(hi.Xuid[:]), gamertag)
	if err != nil {
		logging.Warn.Printf("[%s] profile ensure failed, using static reg: %v", s.serverConfig.Label, err)
		return CreateSquadRegState(hi.Xuid, hi.Order)
	}

	squad, err := s.repo.CreateSquad(readCtx, name, faction, profile)
	if err != nil {
		logging.Warn.Printf("[%s] squad create failed, using static reg: %v", s.serverConfig.Label, err)
		return CreateSquadRegState(hi.Xuid, hi.Order)
	}

	logging.Info.Printf("[%s] created squad %q (%s) faction %q for %s", s.serverConfig.Label, name, squad.TeamID, faction, gamertag)
	return squadRegState(hi.Xuid, hi.Order, '1', squad.TeamID, profile.UserID)
}

func NewSquadRegServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadRegServer {
	s := &squadRegServer{repo: repo}

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
			resp := s.buildReg(hi, *readBuffer)
			buf := make([]byte, constants.SquadRegResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
