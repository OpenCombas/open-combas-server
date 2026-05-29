package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type StaticMessage struct {
	Header MessageHeader
	Body   [500]byte
}

func CreateStaticMessage(xuid [16]byte, order [8]byte, body string) StaticMessage {
	message, err := hex.DecodeString(body)
	if err != nil {
		panic(err)
	}
	zeroPadding := bytes.Repeat([]byte{0x00}, 500-len(message))
	messageBody := append(message, zeroPadding...)
	var fixedLengthMessage [500]byte
	copy(fixedLengthMessage[:], messageBody)
	return StaticMessage{
		Header: CreateHeader(xuid, order),
		Body:   fixedLengthMessage,
	}
}

type staticMessageServer struct {
	*messageServer
}

func NewStaticMessageServer(listenAddress net.IP, serverConfig *config.StaticMessageServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *staticMessageServer {
	s := &staticMessageServer{}

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
			return validateStaticMessagePacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			return CreateStaticMessage(hi.Xuid, hi.Order, serverConfig.BufferContent)
		},
		responseSize: constants.OrderedMessageSize,
	}

	return s
}

func validateStaticMessagePacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
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
