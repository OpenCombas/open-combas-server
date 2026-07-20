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

var gameSeasonValue = [4]byte{0x72, 0x00, 0x00, 0x00}

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
		ServerLocalTime:            createServerTime(serverTime, 0x04),
		ServerMaintenanceStartTime: createServerTime(maintenanceStart, 0x04),
		ServerMaintenanceEndTime:   createServerTime(maintenanceEnd, flag),
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
