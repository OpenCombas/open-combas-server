package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type echoServer struct {
	*messageServer
}

func NewEchoServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *echoServer {
	s := &echoServer{}

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
			return validateEchoPacket(packet, clientAddr, serverConfig.Label)
		},
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			header := CreateHeader(hi.Xuid, hi.Order)

			headerBuf := make([]byte, 32)
			if _, err := binary.Encode(headerBuf, binary.LittleEndian, header); err != nil {
				return nil, err
			}

			payload := (*readBuffer)[32:]
			response := append(headerBuf, payload...)
			return &response, nil
		},
	}

	return s
}

func validateEchoPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
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

	return nil
}
