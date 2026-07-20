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

// memberLoginStallDefault pads the joiner's applicant-teardown -> member-reconnect gap past the host's
// ~10s user-retirement timeout. See the block in buildLogin for the full derivation.
//
// 8s is the measured-good value: the un-stalled gap is ~5.1s, and 8s produced 13.2s end-to-end, clearing
// the 10s deadline with ~3.2s of margin. Do not trim it below ~5s -- that removes the margin entirely.
const memberLoginStallDefault = 8 * time.Second

// memberLoginStall is ON by construction. Set SQUAD_MEMBER_LOGIN_STALL_MS=0 to force it off for a clean
// baseline capture; that direction is safe because a baseline that silently kept the stall is obvious in
// the timings. Any other value overrides the default.
var memberLoginStall = func() time.Duration {
	raw, ok := os.LookupEnv("SQUAD_MEMBER_LOGIN_STALL_MS")
	if !ok {
		return memberLoginStallDefault
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		return memberLoginStallDefault
	}
	return time.Duration(ms) * time.Millisecond
}()

func init() {
	// Logged unconditionally, both states: an absent line must never be ambiguous between "disabled" and
	// "this build predates the change".
	if memberLoginStall > 0 {
		logging.Warn.Printf("squad member login stall ACTIVE (%v) -- the 184 reply to a member who just "+
			"joined via 182 is held once, to outlast the host's ~10s stale-member retirement. Normal logins "+
			"are unaffected. Timing-coupled workaround, see buildLogin.", memberLoginStall)
	} else {
		logging.Warn.Printf("squad member login stall DISABLED via SQUAD_MEMBER_LOGIN_STALL_MS=0 -- " +
			"squad joins are expected to fail with the joiner falling back to the title screen.")
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

// Squad grade index bounds. The client renders the grade as FMG string 5700+idx, so 0 resolves to the
// panel's own "Squad Grade" label and anything past the table runs off the end of it. Grade 1 is the
// lowest rank ("Rookie"); 13 is the highest. See SquadTeamInfo.Grade.
const (
	squadGradeMin = 1
	squadGradeMax = 13
)

// clampSquadGrade maps a stored grade into the renderable range. Squads written before Squad.Grade existed
// decode as 0, which would otherwise display the label text in place of the value.
func clampSquadGrade(g int32) byte {
	if g < squadGradeMin {
		return squadGradeMin
	}
	if g > squadGradeMax {
		return squadGradeMax
	}
	return byte(g)
}

// SquadTeamInfo is the 92-byte team header. Wire schema "C20, I11, C20, I2".
type SquadTeamInfo struct {
	Status      byte     // off 0  - 0 = valid team; non-zero => record zeroed ("no team")
	TeamName    [16]byte // off 1  - Team Name
	CountryCode byte     // off 17 - Country Code ('A' Tarakia / 'B' Morskoj / 'C' Sal Kar)
	// off 18 - MEMBER COUNT. This byte has TWO consumers and they disagree about what it means:
	//   * the session/roster view renders it as the member count (measured in-game 2026-07-20: serving
	//     capture points here displayed "255 members")
	//   * Release.xex sub_821AF778 passes *(u8*)(hdr+18) to sub_821AEA50, which resolves its widget by the
	//     literal name "string_Capture"
	// Both readings are evidenced, so ONE of them is not actually reading this struct -- most likely
	// sub_821AF778 operates on a differently-sourced team record (ranking/search entry) that shares the
	// first 20 bytes, which would also explain why the grade fix at +19 worked. UNRESOLVED; serving the
	// member count because that consumer is confirmed by observed behaviour. Do not "fix" this to capture
	// points again without first establishing which record sub_821AF778 is handed -- see xrefs to it.
	MemberCount byte // off 18
	// off 19 - SQUAD GRADE index. Not padding: the squad panel reads this byte and renders FMG string
	// 5700+idx (Release.xex sub_821AF778 passes *(u8*)(hdr+19) to sub_821ADF48, which does
	// sub_82153738(idx + 5700)). Valid grades are 1..13 -> "Rookie".."". Index 0 resolves to FMG 5700,
	// which is the panel's own LABEL string, so a zero here renders as "Squad Grade    Squad Grade".
	// Grade is the one field in that function confirmed against THIS struct by in-game behaviour: the
	// lobby rendered "Squad Grade  Squad Grade" until +19 was populated. The +18 reading from the same
	// function does NOT hold here (see MemberCount above), so do not treat sub_821AF778 as a blanket
	// authority on this layout.
	Grade byte // off 19 (end of C20)
	// off 20 - RENOWN, not the team rank. sub_821AF778 passes *(i32*)(hdr+20) to sub_821AEAE8, which
	// resolves its widget by the literal name "string_Renown". Serving squad.Rank here is what made the
	// lobby show "Renown 1" for a squad with 444 lifetime renown, while the 202 ranking view -- which
	// carries renown in its own block -- displayed it correctly.
	Renown      int32
	_           int32   // off 24
	Sorties     int32   // off 28 - Number of sorties
	Wins        int32   // off 32 - Number of Win
	Losses      int32   // off 36 - Number of Lose
	ShootDowns  int32   // off 40 - Number of Shoot Down
	ConShootDn  int32   // off 44 - Number of Con Shoot Down
	CombasDown  int32   // off 48 - Number of Combas Down
	CmdBaseDown int32   // off 52 - Number of Command Base Down
	_           int32   // off 56
	_           int32   // off 60 (end of I11)
	Color1      [3]byte // off 64 - Team Color 1 (R,G,B)
	Color2      [3]byte // off 67 - Team Color 2
	Color3      [3]byte // off 70 - Team Color 3
	Color4      [3]byte // off 73 - Team Color 4
	Patern      byte    // off 76 - Team Patern (emblem)
	Stance      byte    // off 77 - parser "Team Profile"; = squad Stance (config setting)
	Activity    byte    // off 78 - parser "Main Play Time"; = Activity Level (config setting)
	Language    byte    // off 79 - Language (config setting)
	Regions     byte    // off 80 - parser "Strategy"; = Connected Regions (config setting)
	RoleFlags   byte    // off 81 - parser "Recruit Type"; = role bitmask (config setting)
	_           [2]byte // off 82 - pad (end of C20)
	_           int32   // off 84
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
	t.TeamInfoCount = 1 // static record: fixed serial (no persistence to bump)
	t.MemberCount = 1
	// The fallback squad has no stats doc, so renown is genuinely 0. Serving squad.Rank here is what made
	// the lobby render "Renown 1".
	t.Renown = 0
	t.Grade = squadGradeMin
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
// stats may be nil (no stats doc yet, or the lookup failed); both lobby counters then serve 0, which is
// what a squad that has never fought actually has.
func squadLoginStateFromSquad(hi UserHelloMessage, squad *Squad, stats *SquadStats) SquadLoginState {
	state := SquadLoginState{Header: CreateHeader(hi.Xuid, hi.Order)}
	t := &state.Data.Team
	t.Status = 0
	copy(t.TeamName[:], squad.Name)
	if len(squad.Faction) > 0 {
		t.CountryCode = squad.Faction[0]
	} else {
		t.CountryCode = 'A'
	}
	// Lobby Renown (off 20) comes from lifetime stats, NOT the ranking position -- sub_821AF778 passes
	// hdr+20 to a widget resolved as "string_Renown". Verified in-game 2026-07-20: a squad with 444
	// lifetime renown now reads "Renown 444" where it previously read "Renown 1" (its rank).
	// Capture points are NOT served here; off 18 is the member count, see SquadTeamInfo.
	if stats != nil {
		t.Renown = stats.Renown.Total
	}
	t.Grade = clampSquadGrade(squad.Grade)
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

	// Hold a NON-LEADER member's 184 reply so their squad-member reconnect lands after the host has retired
	// their stale applicant entry. Timing-coupled by nature -- read this before changing it.
	//
	// MECHANISM. A squad join tears down the applicant P2P session and reconnects as a member (by design;
	// the 0x400B handler's SetState(13) drives it). On that reconnect the host allocates a slot but binds no
	// user while the applicant's entry is still in its table -- duplicate suppression skips `Add User`, which
	// gates the type-07 announce the joiner's link state 6 blocks on. The joiner times out 6->8 and drops to
	// the title screen. Un-stalled the reconnect arrives ~5.1s after the teardown; the host retires the stale
	// entry ~10s after it, so the reconnect is always too early.
	//
	// WHY 10s IS NOT NEGOTIABLE. It is a hard-coded title literal: CDataSegmentRecvWorker (sub_82823990)
	// accumulates elapsed ms whenever the peer socket returns <= 0 and retires the connection at 0x2710
	// (10,000ms). EOF and WOULDBLOCK take the same branch, so a promptly-delivered close does not shorten it
	// -- measured at 9,765 / 9,961 / 10,011 / 9,818 ms across four captures. Only a genuine socket error
	// bypasses the accumulator, and the joiner's teardown is a graceful shutdown()+closesocket() with its
	// receive buffer fully drained (undrained=0B, measured), so reporting an error would fabricate a
	// condition TCP would not produce.
	//
	// WHY NOT RETIRE THE ENTRY SOONER. In this flow the host has exactly one applicable retirement path --
	// ClientDropped on that accumulator. Its other callers are send-failure (the host goes quiet, so
	// unreachable) and two paths gated behind XSessionArbitrationRegister, which does not run during a squad
	// join. No P2P message outside arbitration retires a user.
	//
	// COST AND FAILURE MODE. Adds ~8s to every squad join, and slow session-membership updates are a known
	// bug source here (stale advertised sessions, duplicate lobbies). If the title's 10s constant or the
	// joiner's reconnect timing ever shifts, this fails SILENTLY and looks exactly like the original bug --
	// joiner kicked to the title screen. If that reappears, re-derive the gap before suspecting anything else.
	//
	// Grade is derived from lifetime renown at read time rather than trusted from the squad doc, so it is
	// correct even if a battle credit landed without a RefreshSquadGrade (see squadGrade.go). On a read
	// error the stored value is served instead -- a stale grade beats failing a login over a cosmetic field.
	if grade, err := s.repo.SquadGradeFor(readCtx, teamID); err == nil {
		squad.Grade = int32(grade)
	} else {
		logging.Warn.Printf("[%s] grade lookup failed for %s, serving stored grade: %v", s.serverConfig.Label, teamID, err)
	}

	// SCOPE. Only users flagged by a preceding 182 join are stalled, and only once (the marker is consumed).
	// A member signing in normally -- cold boot, no join in flight -- has no stale applicant entry on any host
	// and must not pay the latency. Leader is excluded on top of that: the host also issues a 184 inside its
	// own join-commit sequence (0x4008 -> 182 -> 184 -> 0x7001 -> 0x400b), and stalling that would stall the
	// join itself.
	if memberLoginStall > 0 {
		xuid := string(hi.Xuid[:])
		if m := memberByXUID(squad, xuid); m != nil && !m.Leader && pendingMemberReconnects.Consume(xuid) {
			logging.Info.Printf("[%s] holding 184 reply for freshly-joined member %s (%s) by %v to outlast host stale-member retirement",
				s.serverConfig.Label, m.Gamertag, teamID, memberLoginStall)
			select {
			case <-time.After(memberLoginStall):
			case <-s.ctx.Done():
			}
		}
	}

	// Lobby Capture/Renown counters come from the stats doc. A squad that has never fought has no doc at
	// all, which is not an error -- serve zeros. A genuine lookup failure is logged but must not fail the
	// login over two cosmetic fields.
	stats, err := s.repo.SquadStatsByTeamID(readCtx, teamID)
	if err != nil {
		logging.Warn.Printf("[%s] stats lookup failed for %s, serving zero capture/renown: %v", s.serverConfig.Label, teamID, err)
		stats = nil
	}

	return squadLoginStateFromSquad(hi, squad, stats)
}

// memberByXUID finds a roster entry by the packet-header XUID (the reliable identity of the caller; the
// body gamertag is not, which is why the roster is keyed on XUID here).
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
