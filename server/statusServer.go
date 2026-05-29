package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"fmt"
	"net"
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

var programVersionValue = [4]byte{0x00, 0x00, 0x10, 0x00}

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

func CreateStatus(xuid [16]byte, order [8]byte, serverTime time.Time, maintenanceStart time.Time, maintenanceEnd time.Time) ServerState {
	return ServerState{
		Header:                     CreateHeader(xuid, order),
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             programVersionValue,
		ServerLocalTime:            createServerTime(serverTime, 0x04),
		ServerMaintenanceStartTime: createServerTime(maintenanceStart, 0x04),
		ServerMaintenanceEndTime:   createServerTime(maintenanceEnd, 0x00),
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
}

func NewStatusServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *statusServer {
	s := &statusServer{}

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
			return validateStatusPacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			startTime := time.Now()
			startOffset := time.Hour * 12
			endOffset := time.Hour * 24
			return CreateStatus(hi.Xuid, hi.Order, startTime, startTime.Add(startOffset), startTime.Add(endOffset))
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
