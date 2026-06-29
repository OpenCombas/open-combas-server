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

	"github.com/prometheus/client_golang/prometheus"
)

// Squad login / squad-data fetch response.
//
// Reverse-engineered from Release.xex: request builder sub_823BEA50 (internal message code 184)
// sends body "<gamertag>,<teamid>" to port 1020+184 = 1204 (and 1244 for the newer revision).
// recordSize is 1248, parser is sub_823BCD88. The client deserializes the 1248-byte body with
// schema "C20,I11,C20,I2" + 16x"S4,C3" + 20x"X1,C37" (verified against the create-squad debug log:
// "Data: 20[20],44[64],20[84],8[92]" then 16x(8,3) then 20x(8,37)).
//
// The parser's first action after deserializing is:
//     if (body[0] != 0) { memset(body, 0, 1248); body[0] = status; }
// i.e. a non-zero status byte means "no team" and zeroes the whole record. A login with an all-zero
// team id (TM0000000000000000, sent at sign-in before any squad exists) must therefore come back with
// a non-zero status; the previous echo stub worked at login only because the echoed body[0] ('i' from
// "ibac") happened to be non-zero. After a squad is created the client re-issues 1204 with the freshly
// assigned team id and expects a populated record -- the echo stub returned garbage there, the parse
// failed, and squad creation aborted with "Communication with the Chromehounds server failed. Cannot
// upload settings."
//
// Field semantics below come from the parser's debug-print labels (sub_823BCD88). See SERVER_ANALYSIS.md.

// SquadEmblem is one emblem layer (12 bytes). Wire schema "S4, C3" (+1 stride pad).
type SquadEmblem struct {
	PaternID int16   // off 0  - Patern ID
	Angle    int16   // off 2  - Angle
	CoordX   int16   // off 4  - Coordinates X
	CoordY   int16   // off 6  - Coordinates Y
	Color    byte    // off 8  - Color
	ScaleX   byte    // off 9  - Expansion&Reduction X
	ScaleY   byte    // off 10 - Expansion&Reduction Y
	_        byte    // off 11 - stride pad
}

// SquadMember is one roster entry (48 bytes). Wire schema "X1, C37" (+3 stride bytes used as rank).
type SquadMember struct {
	XUID       int64    // off 0  - member XUID (binary)
	UserID     [18]byte // off 8  - "US" + 16 digits
	_          byte     // off 26 - pad
	UserName   [16]byte // off 27 - gamertag
	UserNumber byte     // off 43 - User Number
	LeaderFlg  byte     // off 44 - Leader Flag (1 = squad leader)
	Rank       [3]byte  // off 45 - User Rank (3 raw bytes, big-endian assembled)
}

// SquadTeamInfo is the 92-byte team header. Wire schema "C20, I11, C20, I2".
type SquadTeamInfo struct {
	Status      byte     // off 0  - 0 = valid team; non-zero => record zeroed ("no team")
	TeamName    [16]byte // off 1  - Team Name
	CountryCode byte     // off 17 - Country Code ('A' Tarakia / 'B' Morskoj / 'C' Sal Kar)
	MemberCount byte     // off 18 - Number Of Member
	_           byte     // off 19 - pad (end of C20)
	TeamRank    int32    // off 20 - Team Rank
	_           int32    // off 24
	Sorties     int32    // off 28 - Number of sorties
	Wins        int32    // off 32 - Number of Win
	Losses      int32    // off 36 - Number of Lose
	ShootDowns  int32    // off 40 - Number of Shoot Down
	ConShootDn  int32    // off 44 - Number of Con Shoot Down
	CombasDown  int32    // off 48 - Number of Combas Down
	CmdBaseDown int32    // off 52 - Number of Command Base Down
	_           int32    // off 56
	_           int32    // off 60 (end of I11)
	Color1      [3]byte  // off 64 - Team Color 1 (R,G,B)
	Color2      [3]byte  // off 67 - Team Color 2
	Color3      [3]byte  // off 70 - Team Color 3
	Color4      [3]byte  // off 73 - Team Color 4
	Patern      byte     // off 76 - Team Patern
	Profile     byte     // off 77 - Team Profile
	PlayTime    byte     // off 78 - Main Play Time
	Language    byte     // off 79 - Language
	Strategy    byte     // off 80 - Strategy
	RecruitType byte     // off 81 - Recruit Type
	_           [2]byte  // off 82 - pad (end of C20)
	_           int32    // off 84
	_           int32    // off 88 (end of I2)
}

// SquadData is the full 1248-byte squad-login body.
type SquadData struct {
	Team    SquadTeamInfo    // off 0    (92)
	Emblems [16]SquadEmblem  // off 92   (192)
	_       [4]byte          // off 284  pad
	Members [20]SquadMember  // off 288  (960)
}

// SquadLoginState is header(32) + body(1248) = constants.SquadLoginResponseSize (1280).
type SquadLoginState struct {
	Header MessageHeader
	Data   SquadData
}

// parseSquadLogin extracts the gamertag and team id from a body of "<gamertag>,<teamid>".
func parseSquadLogin(packet []byte) (gamertag string, teamID string) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", ""
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.SplitN(string(body), ",", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	if len(parts) == 1 {
		return strings.TrimSpace(parts[0]), ""
	}
	return "", ""
}

// teamIDIsEmpty reports whether a team id is absent (all-zero digits or empty), meaning the player
// is not in a squad yet.
func teamIDIsEmpty(teamID string) bool {
	digits := strings.TrimPrefix(teamID, "TM")
	if digits == "" {
		return true
	}
	return strings.Trim(digits, "0") == ""
}

// xuidToInt64 converts the 16-char ASCII-hex XUID from the message header into its numeric value so it
// can be matched against the client's own XUID in the roster.
func xuidToInt64(xuid [16]byte) int64 {
	v, err := strconv.ParseUint(strings.TrimSpace(string(xuid[:])), 16, 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

func CreateSquadLoginState(hi UserHelloMessage, packet []byte) SquadLoginState {
	gamertag, teamID := parseSquadLogin(packet)

	state := SquadLoginState{Header: CreateHeader(hi.Xuid, hi.Order)}

	// No squad yet: a non-zero status byte makes the client treat the record as "no team".
	if teamIDIsEmpty(teamID) {
		state.Data.Team.Status = 1
		return state
	}

	// STATIC squad record. Until a persistent squad store exists, the server cannot recall the name /
	// faction that were submitted at registration, so the team identity is hard-coded to match the
	// squad the user creates in testing ("OpenCombas", Republic of Morskoj / 'B'). The roster's single
	// member is built dynamically from the requester so the client recognises itself as the leader and
	// proceeds to upload settings. See project_combas_server_protocol memory.
	t := &state.Data.Team
	t.Status = 0
	copy(t.TeamName[:], "OpenCombas")
	t.CountryCode = 'B'
	t.MemberCount = 1
	t.TeamRank = 1
	t.Language = 'J'
	t.Color1 = [3]byte{0xFF, 0x00, 0x00}

	m := &state.Data.Members[0]
	m.XUID = xuidToInt64(hi.Xuid)
	copy(m.UserID[:], "US0001000000000001") // must match the id assigned by the squad-reg (1201) server
	copy(m.UserName[:], gamertag)
	m.UserNumber = 1
	m.LeaderFlg = 1
	m.Rank = [3]byte{0x00, 0x00, 0x01} // rank 1 (3 bytes, big-endian)

	return state
}

type squadLoginServer struct {
	*messageServer
}

func NewSquadLoginServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *squadLoginServer {
	s := &squadLoginServer{}

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
			resp := CreateSquadLoginState(hi, *readBuffer)
			buf := make([]byte, constants.SquadLoginResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
