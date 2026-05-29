package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type UserHelloMessage struct {
	ChromeHounds     [4]byte
	Xuid             [16]byte
	Order            [8]byte
	HeaderTerminator [4]byte
}

type MessageHeader struct {
	ChromeHounds     [4]byte
	Xuid             [16]byte
	Order            [8]byte
	HeaderTerminator [4]byte
}

var chromeHoundsHeaderValue = [4]byte{'C', 'H', 0x0, 0x0}

var headerTerminatorValue = [4]byte{
	0x00, 0x00, 0x00, 0x00,
}

var XuidValueHardCoded = [16]byte{
	'0', '0', '0', '9', '0', '0', '0', '0',
	'4', 'E', 'A', '2', '5', '0', '6',
	'3'}

func CreateHeader(xuid [16]byte, order [8]byte) MessageHeader {
	return MessageHeader{
		ChromeHounds:     chromeHoundsHeaderValue,
		Xuid:             xuid,
		Order:            order,
		HeaderTerminator: headerTerminatorValue,
	}
}

type messageServer struct {
	listenAddress net.IP
	serverConfig  *config.ServerConfig
	bufferSize    int
	loggingConfig *config.LoggingConfig
	ctx           context.Context
	wg            *sync.WaitGroup
	promConfig    config.PrometheusConfig
	reg           prometheus.Registerer

	validatePacket func(packet []byte, clientAddr *net.UDPAddr) error
	buildPayload   func(hi UserHelloMessage) interface{}
	buildResponse  func(readBuffer *[]byte) (*[]byte, error)
	responseSize   int
}

func (s *messageServer) Run() {
	statusResponsesHandled := promauto.With(s.reg).NewCounter(prometheus.CounterOpts{
		Name: "status_responses_handled_total",
		Help: "Total number of status responses handled",
	})
	s.wg.Add(1)
	defer s.wg.Done()
	enablePerfMonitoring := s.loggingConfig.EnablePerformanceMonitoring
	verboseLogging := s.loggingConfig.Verbose
	label := s.serverConfig.Label

	conn, err := buildUDPListener(s.listenAddress, s.serverConfig.Port, label, s.bufferSize)
	if err != nil {
		return
	}
	defer conn.Close()

	readBuffer := pooling.ReadBufferPool.Get()
	defer pooling.ReadBufferPool.Put(readBuffer)

	var startTime time.Time
	var processingTime time.Duration

	for {
		select {
		case <-s.ctx.Done():
			if verboseLogging {
				logging.LogShutdown(label)
			}
			return

		default:
			if enablePerfMonitoring {
				startTime = time.Now()
			}

			n, clientAddr, err := readUDP(conn, &readBuffer, label)
			if err != nil {
				if !isTimeoutError(err) && enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			packet := readBuffer[:n]

			if err := s.validatePacket(packet, clientAddr); err != nil {
				if verboseLogging {
					logging.LogPacketValidationError(label, clientAddr, err.Error(), n)
				}
				if enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			if enablePerfMonitoring {
				processingTime = time.Since(startTime)
				profiling.RecordPacketProcessed(n, processingTime)
			}
			if verboseLogging {
				logging.LogPacketReceived(label, clientAddr, n, processingTime)
			}

			sendBuffer, err := s.createOrderedResponse(&packet)
			if err != nil {
				if verboseLogging {
					logging.Warn.Println(err)
				}
				if enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			sendUDP(conn, clientAddr, sendBuffer, label, true)
			if s.promConfig.Enabled {
				statusResponsesHandled.Inc()
			}
		}
	}
}

func (s *messageServer) parseHelloMessage(readBuffer *[]byte) UserHelloMessage {
	label := s.serverConfig.Label
	var helloBuffer []byte = (*readBuffer)[:constants.MinHelloMessageSize]
	var helloStruct UserHelloMessage

	if _, err := binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct); err != nil {
		logging.Warn.Printf("[%s] fallback to default xuid due to parsing error of hello header: %v\n", label, err)
		helloStruct.Xuid = XuidValueHardCoded
	}

	return helloStruct
}

func (s *messageServer) createOrderedResponse(readBuffer *[]byte) (*[]byte, error) {
	var startTime = time.Now()
	label := s.serverConfig.Label
	enablePerformanceMonitoring := s.loggingConfig.EnablePerformanceMonitoring

	if s.buildResponse != nil {
		result, err := s.buildResponse(readBuffer)
		if err == nil && enablePerformanceMonitoring {
			processingTime := time.Since(startTime)
			logging.LogPerformanceMetric(label, "response_creation", processingTime)
		}
		return result, err
	}

	helloStruct := s.parseHelloMessage(readBuffer)
	responseStruct := s.buildPayload(helloStruct)

	sendBuffer := pooling.StatusResponsePool.Get()
	defer pooling.StatusResponsePool.Put(sendBuffer)

	responseBuffer := make([]byte, s.responseSize)

	if _, err := binary.Encode(responseBuffer, binary.LittleEndian, responseStruct); err != nil {
		logging.Warn.Printf("[%s] Error populating sendbuffer: %s", label, err)
		return nil, err
	}

	if enablePerformanceMonitoring {
		processingTime := time.Since(startTime)
		logging.LogPerformanceMetric(label, "response_creation", processingTime)
	}

	return &responseBuffer, nil
}
