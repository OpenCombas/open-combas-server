package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type ServerTime struct {
	Year   uint16
	Month  uint8
	Day    uint8
	Hour   uint8
	Minute uint8
	Second uint8
	Flag   byte
}

type ServerState struct {
	Header                     MessageHeader
	GameSeason                 [4]byte
	ProgramVersion             [4]byte
	ServerLocalTime            ServerTime
	ServerMaintenanceStartTime ServerTime
	ServerMaintenanceEndTime   ServerTime
}

// EXPERIMENT KNOBS for the 32-byte status body. Every byte of the reply is now settable so the unexplored
// fields can be swept without a code change. All values are logged unconditionally at startup, so a capture
// can never be ambiguous about what was on the wire.
//
// Body layout (netmgr+2828 once stored; schema "S,C2,I,S,C6,S,C6,S,C6", read LITTLE-endian):
//
//	+0  GameSeason u16      COMBAS_GAME_SEASON      season id (114 today)
//	+2  byte                COMBAS_SEASON_BYTE2     -> netmgr+24 = 1000*v, combas request retry timeout ms
//	+3  byte                COMBAS_SEASON_BYTE3     -> netmgr+28, retry related
//	+4  ProgramVersion u32  COMBAS_PROGRAM_VERSION  must be 0x00100000 or the client reports "unavailable"
//	+15 LocalTime.Flag      COMBAS_FLAG_LOCAL       no reader found in 768KB of menu+net code
//	+23 MaintStart.Flag     COMBAS_FLAG_START       no reader found either
//	+31 MaintEnd.Flag       COMBAS_FLAG_END         SERVER UP/DOWN -> netmgr+8; non-zero = "!! DOWN !!"
//
// The two 0x04s have NO recorded provenance -- SERVER_ANALYSIS.md shows they were simply chosen by the
// first implementation, not taken from a capture of the original service. No reader for either was found
// across 768KB of menu + network code, so they appear inert.
//
// Full RE reference for this message: workspaces/chromehounds_status_re.md. The maintenance login popup is
// BY DESIGN with no server-side lever -- every avenue was measured and closed on 2026-07-20 (see section 6
// of that document) -- so please read it before spending time here again.
func envByte(name string, def byte) byte {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(raw), "0x"), 16, 8)
	if err != nil {
		logging.Warn.Printf("%s=%q is not a hex byte, using default 0x%02X", name, raw, def)
		return def
	}
	return byte(v)
}

var (
	gameSeasonID = func() uint16 {
		raw, ok := os.LookupEnv("COMBAS_GAME_SEASON")
		if !ok {
			return 0x0072 // 114
		}
		v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 16)
		if err != nil {
			logging.Warn.Printf("COMBAS_GAME_SEASON=%q is not a number, using 114", raw)
			return 0x0072
		}
		return uint16(v)
	}()
	seasonByte2 = envByte("COMBAS_SEASON_BYTE2", 0x00)
	seasonByte3 = envByte("COMBAS_SEASON_BYTE3", 0x00)
	flagLocal   = envByte("COMBAS_FLAG_LOCAL", 0x04)
	flagStart   = envByte("COMBAS_FLAG_START", 0x04)
	flagEnd     = envByte("COMBAS_FLAG_END", 0x00)

	// COMBAS_MAINT_ZERO=1 sends an ALL-ZERO maintenance start+end (year 0, month 0, ...) instead of real
	// dates. Untested hypothesis: a zeroed window may be the service's "nothing scheduled" sentinel.
	//
	// What the client does with it is genuinely unknown. sub_823B5FC8 feeds both times through mktime
	// (sub_823B5968: tm.year = year-1900, tm.mon = month-1), and year 0 / month 0 yields tm_year = -1900,
	// tm_mon = -1, which mktime should reject with -1. The comparisons are UNSIGNED, so -1 becomes
	// 0xFFFFFFFF and every "now < start" test succeeds -- predicting all three flags stay clear, i.e. the
	// same state a future window produces. If so this changes nothing; if the client special-cases the
	// sentinel earlier, it is the toggle we have been looking for.
	maintZero = envByte("COMBAS_MAINT_ZERO", 0x00) != 0
)

// zeroServerTime is the all-zero sentinel used when COMBAS_MAINT_ZERO is set. The flag byte is preserved so
// the end flag still carries server up/down.
func zeroServerTime(flag byte) ServerTime {
	return ServerTime{Flag: flag}
}

// gameSeasonValue is the "S,C2" field: LE season id then the two tunable bytes.
var gameSeasonValue = func() [4]byte {
	var b [4]byte
	binary.LittleEndian.PutUint16(b[0:2], gameSeasonID)
	b[2], b[3] = seasonByte2, seasonByte3
	return b
}()

func init() {
	logging.Warn.Printf("status body knobs: season=%d byte2=0x%02X byte3=0x%02X flags local=0x%02X "+
		"start=0x%02X end=0x%02X maintZero=%v%s", gameSeasonID, seasonByte2, seasonByte3, flagLocal,
		flagStart, flagEnd, maintZero,
		map[bool]string{true: "  [end flag NON-ZERO => client reports server DOWN]", false: ""}[flagEnd != 0])
}

// programVersionExpected is the value the client requires. Release.xex sub_823B6918 (the status-response
// handler) branches on it:
//
//	if (*(netmgr + 2832) != 0x100000) { netmgr[+12] = 1; return 0; }   // maintenance NEVER evaluated
//	netmgr[+12] = 5;                                                    // state 5 runs the maintenance check
//
// State 5 is what drives sub_823B5FC8, which sets the three maintenance flags. So a mismatch does suppress
// the maintenance evaluation entirely -- but netmgr state 1 is ALSO what sub_823B5CE8 sets on a socket bind
// failure, and the netmgr tick skips states 1 and 6, so state 1 looks like a terminal error rather than a
// graceful skip. Expect login to break rather than merely go quiet.
const programVersionExpected = 0x00100000

// programVersion is settable to test whether the version field was repurposed as a "check maintenance"
// switch (the log strings around it still say "Program Version", but the log text may simply never have
// been updated). Set COMBAS_PROGRAM_VERSION to a hex value, e.g. COMBAS_PROGRAM_VERSION=0 .
var programVersion = func() uint32 {
	raw, ok := os.LookupEnv("COMBAS_PROGRAM_VERSION")
	if !ok {
		return programVersionExpected
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(raw), "0x"), 16, 32)
	if err != nil {
		logging.Warn.Printf("COMBAS_PROGRAM_VERSION=%q is not hex, using default 0x%08X", raw, uint32(programVersionExpected))
		return programVersionExpected
	}
	return uint32(v)
}()

// Logged unconditionally so a capture can never be ambiguous about which value was on the wire.
func init() {
	if programVersion == programVersionExpected {
		logging.Warn.Printf("program version = 0x%08X (matches client expectation; client enters netmgr "+
			"state 5 and DOES evaluate the maintenance window)", programVersion)
	} else {
		logging.Warn.Printf("program version = 0x%08X -- MISMATCH vs the expected 0x%08X. EXPERIMENT: the "+
			"client diverts to netmgr state 1 and never evaluates maintenance. State 1 is also its "+
			"socket-bind-failure state, so login may break outright rather than going quiet.",
			programVersion, uint32(programVersionExpected))
	}
}

// LITTLE-endian: the client's schema deserializer reads multi-byte fields LE, which is why the historical
// [4]byte{0x00,0x00,0x10,0x00} reads back as 0x00100000 in its "Program Ver = 0x%08X" log. Same reason the
// GameSeason short 0x72,0x00 prints as season 114.
var programVersionValue = func() [4]byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], programVersion)
	return b
}()

func CreateServerTimeRaw(year uint16, month uint8, day uint8, hour uint8, minute uint8, second uint8, flag byte) ServerTime {
	return ServerTime{
		Year:   year,
		Month:  month,
		Day:    day,
		Hour:   hour,
		Minute: minute,
		Second: second,
		Flag:   flag,
	}
}

// orDefaultByte lets a caller pin the end flag explicitly while 0 defers to the env-configured value.
func orDefaultByte(v, def byte) byte {
	if v != 0 {
		return v
	}
	return def
}

// maintTime renders a maintenance timestamp, honouring the COMBAS_MAINT_ZERO sentinel experiment.
func maintTime(t time.Time, flag byte) ServerTime {
	if maintZero {
		return zeroServerTime(flag)
	}
	return createServerTime(t, flag)
}

func createServerTime(time time.Time, flag byte) ServerTime {
	return ServerTime{

		Year:   uint16(time.Year()),
		Month:  uint8(time.Month()),
		Day:    uint8(time.Day()),
		Hour:   uint8(time.Hour()),
		Minute: uint8(time.Minute()),
		Second: uint8(0x00),
		Flag:   flag,
	}
}

func CreateStatus(xuid [16]byte, order [8]byte, serverTime time.Time, maintenanceStart time.Time, maintenanceEnd time.Time, flag byte) ServerState {
	return ServerState{
		Header:                     CreateHeader(xuid, order),
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             programVersionValue,
		ServerLocalTime:            createServerTime(serverTime, flagLocal),
		ServerMaintenanceStartTime: maintTime(maintenanceStart, flagStart),
		// `flag` is the caller-supplied override; 0 means "use the configured COMBAS_FLAG_END".
		ServerMaintenanceEndTime: maintTime(maintenanceEnd, orDefaultByte(flag, flagEnd)),
	}
}

func CreateStatusRaw(xuid [16]byte, order [8]byte, local ServerTime, maintStart ServerTime, maintEnd ServerTime) ServerState {
	return ServerState{
		Header:                     CreateHeader(xuid, order),
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             programVersionValue,
		ServerLocalTime:            local,
		ServerMaintenanceStartTime: maintStart,
		ServerMaintenanceEndTime:   maintEnd,
	}
}

type statusServer struct {
	*messageServer
	worldRepo *WorldRepository
}

func NewStatusServer(listenAddress net.IP, serverConfig config.StatusServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, worldRepo *WorldRepository) *statusServer {
	s := &statusServer{worldRepo: worldRepo}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig.ServerConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateStatusPacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			now := time.Now()
			if serverConfig.IsResetting {
				// Deliberately "in maintenance" (start <= now <= end): the client shows the in-progress
				// announce and keeps players off a resetting world. See maintenanceWindow.go for the state
				// table this relies on.
				return CreateStatus(hi.Xuid, hi.Order, now, now.Add(-12*time.Hour), now.Add(24*time.Hour), 0x00)
			}
			// Otherwise serve the scheduled window, defaulting to one already in the past so the client
			// computes "maintenance ended" and announces NOTHING. A future-dated window -- which is what
			// this used to send -- makes the client nag about it at every login regardless of distance.
			readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
			defer cancel()
			start, end := s.worldRepo.MaintenanceWindowFor(readCtx, now)
			return CreateStatus(hi.Xuid, hi.Order, now, start, end, 0x00)
		},
		responseSize: constants.StatusResponseSize,
	}

	return s
}

func validateStatusPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
	packetSize := len(packet)

	if packetSize < constants.MinHelloMessageSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too small (minimum: %d bytes)", constants.MinHelloMessageSize),
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	if packetSize > constants.MaxBufferSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too large (maximum: %d bytes)", constants.MaxBufferSize),
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	expectedHeader := ChromeHoundsHeader
	if packet[0] != expectedHeader[0] || packet[1] != expectedHeader[1] {
		err := ValidationError{
			Reason: "invalid Chromehounds header",
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	return nil
}
