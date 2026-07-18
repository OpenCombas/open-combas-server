package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// teamInfoSeq is a process-wide MONOTONIC counter serialized into each login response's TeamInfoCount
// (team-header off 88). The client (Release.xex sub_823C0ED8) keeps a high-water mark of the last count it
// saw and only accepts + peer-rebroadcasts a roster whose count EXCEEDS it. So the count MUST strictly
// increase on every response, or a reconnect with an unchanged roster reads as "OLD" and never re-propagates
// its identity (observed: member count fluctuates 2->1->2 and the high-water mark pins it -> reconnect
// stalls -> lose XBL). Seeded from the wall clock so it also outruns any value a client cached before a
// server restart; bumped once per response.
var teamInfoSeq = int32(time.Now().Unix())

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
	PaternID int16 // off 0  - Patern ID
	Angle    int16 // off 2  - Angle
	CoordX   int16 // off 4  - Coordinates X
	CoordY   int16 // off 6  - Coordinates Y
	Color    byte  // off 8  - Color
	ScaleX   byte  // off 9  - Expansion&Reduction X
	ScaleY   byte  // off 10 - Expansion&Reduction Y
	_        byte  // off 11 - stride pad
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
	Patern      byte     // off 76 - Team Patern (emblem)
	Stance      byte     // off 77 - parser "Team Profile"; = squad Stance (config setting)
	Activity    byte     // off 78 - parser "Main Play Time"; = Activity Level (config setting)
	Language    byte     // off 79 - Language (config setting)
	Regions     byte     // off 80 - parser "Strategy"; = Connected Regions (config setting)
	RoleFlags   byte     // off 81 - parser "Recruit Type"; = role bitmask (config setting)
	_           [2]byte  // off 82 - pad (end of C20)
	_           int32    // off 84
	// off 88 - TEAM-INFO FRESHNESS COUNTER. The client's team-info ingest (Release.xex sub_823C0ED8) reads
	// this and accepts + REBROADCASTS the roster to peers (P2P msg 0x7001) only when it EXCEEDS the value it
	// cached from the previous fetch ("VALID"); otherwise it logs "It is OLD" and discards. Left unset (0)
	// the client judged every fetch stale and never propagated the roster -- a likely cause of the squad
	// roster never reaching the P2P session. Set below to the member count so a join (count rises) makes the
	// fetch VALID. (Follow-up: a monotonic per-squad update-seq would also cover leaves/config changes.)
	TeamInfoCount int32 // off 88 (end of I2)
}

// SquadData is the full 1248-byte squad-login body.
type SquadData struct {
	Team    SquadTeamInfo   // off 0    (92)
	Emblems [16]SquadEmblem // off 92   (192)
	_       [4]byte         // off 284  pad
	Members [20]SquadMember // off 288  (960)
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
	return xuidHexToInt64(string(xuid[:]))
}

// xuidHexToInt64 parses a 16-char ASCII-hex XUID string into its numeric value.
func xuidHexToInt64(s string) int64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 16, 64)
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
	t.TeamInfoCount = atomic.AddInt32(&teamInfoSeq, 1) // monotonic per-response (see teamInfoSeq) -> always VALID
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

// squadLoginStateFromSquad builds the reply from a persisted squad (Phase 2). The requester's header
// xuid/order are echoed; the name, faction and roster come from Mongo. Team-level settings (language,
// colours, stance) stay at defaults until the squad-config (1205) upload is persisted.
func squadLoginStateFromSquad(hi UserHelloMessage, squad *Squad) SquadLoginState {
	state := SquadLoginState{Header: CreateHeader(hi.Xuid, hi.Order)}
	t := &state.Data.Team
	t.Status = 0
	copy(t.TeamName[:], squad.Name)
	if len(squad.Faction) > 0 {
		t.CountryCode = squad.Faction[0]
	} else {
		t.CountryCode = 'A'
	}
	t.TeamRank = squad.Rank
	t.Language = 'J'
	t.Color1 = [3]byte{0xFF, 0x00, 0x00}

	// Surface the settings uploaded via 1205/1245. The five Set-Squad-Profile rows map to consecutive
	// TeamInfo bytes [77..81] (confirmed in-game: Activity[78] and Language[79] round-trip exactly, so by
	// adjacency Stance[77], Regions[80], RoleFlags[81]). Values pass through verbatim - the client packs
	// and unpacks the same enums - so no value translation is needed. Colours map RGB->RGB into Color1.
	if cfg := squad.Settings; cfg != nil {
		// Colours are the full 4-RGB palette (12 bytes) plus a palette selector. Tolerate the legacy
		// 3-byte single-colour form so older squad docs still render Color1.
		if len(cfg.Colors) >= 3 {
			t.Color1 = [3]byte{cfg.Colors[0], cfg.Colors[1], cfg.Colors[2]}
		}
		if len(cfg.Colors) >= 12 {
			t.Color2 = [3]byte{cfg.Colors[3], cfg.Colors[4], cfg.Colors[5]}
			t.Color3 = [3]byte{cfg.Colors[6], cfg.Colors[7], cfg.Colors[8]}
			t.Color4 = [3]byte{cfg.Colors[9], cfg.Colors[10], cfg.Colors[11]}
		}
		t.Patern = byte(cfg.Patern)
		t.Stance = byte(cfg.Stance)
		t.Activity = byte(cfg.Activity)
		if cfg.Language != 0 {
			t.Language = byte(cfg.Language)
		}
		t.Regions = byte(cfg.Regions)
		t.RoleFlags = byte(cfg.RoleFlags)
	}

	// Surface the squad emblem (uploaded via 1206/1246) -- the 192-byte blob is already in the wire
	// "S4,C3" layout, so decoding it straight into the 16-layer emblem array round-trips it.
	if len(squad.Emblems) == squadEmblemBlobSize {
		_, _ = binary.Decode(squad.Emblems, binary.LittleEndian, &state.Data.Emblems)
	}

	n := len(squad.Members)
	if n > len(state.Data.Members) {
		n = len(state.Data.Members) // wire holds 20 members
	}
	t.MemberCount = byte(n)
	// MONOTONIC per-response counter (NOT the member count -- that fluctuates and the client's high-water
	// mark pins it, so a reconnect reads "OLD" and never re-propagates). A strictly-increasing value makes
	// EVERY fetch, including the applicant->member reconnect, "VALID" -> the client re-accepts + rebroadcasts
	// the roster (msg 0x7001) so its identity reaches peers.
	t.TeamInfoCount = atomic.AddInt32(&teamInfoSeq, 1)
	for i := 0; i < n; i++ {
		rec := squad.Members[i]
		m := &state.Data.Members[i]
		m.XUID = xuidHexToInt64(rec.XUID)
		copy(m.UserID[:], rec.UserID)
		// The wire member "UserName" (parser sub_823BCD88 "User Name" field) is the in-squad PILOT NAME
		// -- the value a joining console appears to match its OWN roster record on to derive its assigned
		// US/TM for the vport-1002 lobby handshake. Send rec.Name (set from the 182 join body), NOT the
		// Live gamertag: the host mis-sources the 182 gamertag from the squad lead, so a gamertag-keyed
		// name breaks the joiner's self-match -> null US/TM -> lobby merge never commits. Fall back to the
		// gamertag only when Name is empty (e.g. the leader, whose reg-supplied pilot name isn't persisted).
		name := rec.Name
		if name == "" {
			name = rec.Gamertag
		}
		copy(m.UserName[:], name)
		if rec.Leader {
			m.LeaderFlg = 1
		}
		m.UserNumber = byte(rec.UserNumber)
		m.Rank = rank3(rec.Rank)
	}
	return state
}

// rank3 encodes a rank as the 3 big-endian bytes the client reads at member offset 45..47.
func rank3(rank int32) [3]byte {
	return [3]byte{byte(rank >> 16), byte(rank >> 8), byte(rank)}
}

// noTeamLoginState is the "no team" reply (status byte non-zero makes the client treat the record as
// empty), used for the sign-in probe and for unknown team ids.
func noTeamLoginState(hi UserHelloMessage) SquadLoginState {
	state := SquadLoginState{Header: CreateHeader(hi.Xuid, hi.Order)}
	state.Data.Team.Status = 1
	return state
}

type squadLoginServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> static squad record
}

// buildLogin serves the squad-data reply from Mongo when wired: it looks the team up by id and returns
// the persisted roster. An empty team id (the sign-in probe) or an unknown team yields "no team"; any
// read error falls back to the static record so login never breaks.
func (s *squadLoginServer) buildLogin(hi UserHelloMessage, packet []byte) SquadLoginState {
	if s.repo == nil {
		return CreateSquadLoginState(hi, packet)
	}

	gamertag, teamID := parseSquadLogin(packet)

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()

	// The 184 body carries the caller's OWN gamertag (reliable) -- refresh the profile + roster entry so a
	// joiner whose profile was frozen to a mis-sourced tag at 182-join time is corrected when they log in.
	s.repo.RefreshGamertag(readCtx, string(hi.Xuid[:]), gamertag)

	if teamIDIsEmpty(teamID) {
		return noTeamLoginState(hi)
	}
	squad, err := s.repo.SquadByTeamID(readCtx, teamID)
	if err != nil {
		logging.Warn.Printf("[%s] mongo lookup failed, using static record: %v", s.serverConfig.Label, err)
		return CreateSquadLoginState(hi, packet)
	}
	if squad == nil {
		// Client holds a team id we don't know (e.g. a save from before the DB existed); report no team
		// so it reconciles rather than serving a stale fabricated squad.
		logging.Warn.Printf("[%s] unknown team id %q -> no team", s.serverConfig.Label, teamID)
		return noTeamLoginState(hi)
	}
	return squadLoginStateFromSquad(hi, squad)
}

func NewSquadLoginServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadLoginServer {
	s := &squadLoginServer{repo: repo}

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
			resp := s.buildLogin(hi, *readBuffer)
			buf := make([]byte, constants.SquadLoginResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
