package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// joinerLoginDelay is a TEMPORARY DIAGNOSTIC (env JOINER_LOGIN_DELAY_MS, default 0 = off).
// See the block in buildLogin for what it does and why it must never ship enabled.
var joinerLoginDelay = func() time.Duration {
	ms, err := strconv.Atoi(os.Getenv("JOINER_LOGIN_DELAY_MS"))
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}()

func init() {
	if joinerLoginDelay > 0 {
		logging.Warn.Printf("JOINER_LOGIN_DELAY_MS=%v -- squad login (184) replies to NON-LEADER members are "+
			"deliberately stalled. DIAGNOSTIC ONLY: this pads the applicant-teardown -> member-reconnect gap "+
			"to test whether the host retires the stale user first. It makes the server SLOWER and must not "+
			"ship enabled.", joinerLoginDelay)
	}
}

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
	// off 88 - TEAM-INFO UPDATE SERIAL. The client's team-info ingest (Release.xex sub_823C0ED8) accepts +
	// REBROADCASTS the roster to peers (P2P msg 0x7001) only when this EXCEEDS the value it cached ("VALID");
	// otherwise "OLD" and discarded. Fed from Squad.UpdateSeq (bumped on real squad mutations), so a genuine
	// change propagates while a stable re-login is "OLD" and not re-processed. (Left 0 = never VALID; an
	// always-increasing value = always VALID, which force-processed every login into the client's team-data
	// validation and threw "Incorrect team data" for non-active/squadless consoles -- hence the per-squad serial.)
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
	t.TeamInfoCount = 1 // static record: fixed serial (no persistence to bump)
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
	// Per-SQUAD update serial (bumped only on real squad mutations; see Squad.UpdateSeq). The client
	// (Release.xex sub_823C0ED8) accepts + peer-rebroadcasts the roster only when this EXCEEDS the value it
	// cached, so a genuine change (join/leave/config) reads "VALID" and propagates, while a stable re-login
	// reads the same serial -> "OLD" and is NOT re-processed (which is what stopped the client force-running
	// its team-data validation on every login and throwing "Incorrect team data").
	t.TeamInfoCount = squad.UpdateSeq
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

	// ⚠ TEMPORARY DIAGNOSTIC (env JOINER_LOGIN_DELAY_MS, default 0 = off). NOT A FIX -- see below.
	//
	// Delays ONLY a non-leader member's 184 login reply. The joiner performs this login immediately before
	// reconnecting as a squad member, so stalling it pushes the member reconnect later in wall-clock time.
	//
	// PURPOSE: confirm the causal chain end-to-end. `watchpoints_4` contains a matched pair -- join #1
	// SUCCEEDED with a 13,835ms teardown->reconnect gap, join #2 FAILED at 5,085ms -- against the host's
	// ~10s ClientDropped timeout. The model says the success occurred purely because the host had already
	// retired the applicant's user entry before the member reconnect was accepted. If padding this gap past
	// ~10s reproduces a held session, the model is confirmed by construction rather than by observation of
	// an accident.
	//
	// WHY THIS MUST NOT SHIP: it makes the server slower, and slow session-membership updates are already a
	// known source of real bugs here (players joining sessions the backend still advertises after the host
	// left; duplicate/parallel sessions). The correct direction is to make the host retire the stale member
	// FASTER, not to make the joiner return LATER. This exists to prove the mechanism, then comes straight
	// back out.
	//
	// Leader is excluded deliberately: the HOST also issues a 184 inside its join-commit sequence
	// (0x4008 -> 182 -> 184 -> 0x7001 -> 0x400b), and delaying that would stall the join itself.
	if joinerLoginDelay > 0 {
		if m := memberByXUID(squad, string(hi.Xuid[:])); m != nil && !m.Leader {
			logging.Warn.Printf("[%s] JOINER_LOGIN_DELAY_MS diagnostic: stalling 184 reply for non-leader %s (%s) by %v",
				s.serverConfig.Label, m.Gamertag, teamID, joinerLoginDelay)
			select {
			case <-time.After(joinerLoginDelay):
			case <-s.ctx.Done():
			}
		}
	}

	return squadLoginStateFromSquad(hi, squad)
}

// memberByXUID finds a roster entry by the packet-header XUID (the reliable identity of the caller).
func memberByXUID(squad *Squad, xuid string) *SquadMemberRecord {
	for i := range squad.Members {
		if squad.Members[i].XUID == xuid {
			return &squad.Members[i]
		}
	}
	return nil
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
