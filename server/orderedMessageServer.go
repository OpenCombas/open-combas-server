package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
	"ChromehoundsStatusServer/status"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

func RunOrderedMessageServer(listenAddress net.IP, serverConfig *config.BufferServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) {
	statusResponsesHandled := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "status_responses_handled_total",
		Help: "Total number of status responses handled",
	})
	wg.Add(1)
	defer wg.Done()
	// Pre-compute config flags to avoid pointer dereferencing in hot path
	enablePerfMonitoring := loggingConfig.EnablePerformanceMonitoring
	verboseLogging := loggingConfig.Verbose
	label := serverConfig.Label

	conn, err := buildUDPListener(listenAddress, serverConfig.Port, serverConfig.Label, bufferSize)
	if err != nil {
		return
	}
	defer conn.Close()

	readBuffer := pooling.ReadBufferPool.Get()
	defer pooling.ReadBufferPool.Put(readBuffer)

	// Pre-allocate to avoid repeated allocations
	var startTime time.Time
	var processingTime time.Duration

	for {
		select {
		case <-ctx.Done():
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

			// Validate status packet
			if err := ValidateOrderedMessagePacket(packet, clientAddr, label); err != nil {
				if verboseLogging {
					logging.LogPacketValidationError(label, clientAddr, err.Error(), n)
				}
				if enablePerfMonitoring {
					profiling.RecordError()
				}
				continue // Skip invalid packets
			}

			if enablePerfMonitoring {
				processingTime := time.Since(startTime)
				profiling.RecordPacketProcessed(n, processingTime)
			}
			if verboseLogging {
				logging.LogPacketReceived(label, clientAddr, n, processingTime)
			}

			sendBuffer, err := createOrderedResponse(&packet, label, serverConfig.BufferContent, enablePerfMonitoring)
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
			if promConfig.Enabled {
				statusResponsesHandled.Inc()
			}
		}
	}
}

func createOrderedResponse(readBuffer *[]byte, label string, bufferContent string, enablePerformanceMonitoring bool) (*[]byte, error) {
	var startTime = time.Now()

	var helloBuffer []byte = (*readBuffer)[:32]
	var helloStruct status.UserHelloMessage

	if _, err := binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct); err != nil {
		logging.Warn.Printf("[%s] fallback to default xuid due to parsing error of hello header: %v\n", label, err)
		helloStruct.Xuid = status.XuidValueHardCoded
	}

	responseStruct := status.CreateOrderedMessage(helloStruct.Xuid, helloStruct.Order, bufferContent)

	// Use buffer pool for response
	sendBuffer := pooling.StatusResponsePool.Get()
	defer pooling.StatusResponsePool.Put(sendBuffer)

	// Create a copy for return since we're putting the buffer back in the pool
	// I know this is unsafe, but this server's purpose is only for experimentation
	responseBuffer := make([]byte, constants.OrderedMessageSize)

	if _, err := binary.Encode(responseBuffer, binary.LittleEndian, responseStruct); err != nil {
		logging.Warn.Printf("[%s] Error populating sendbuffer: %s", label, err)
		return nil, err
	}

	if enablePerformanceMonitoring {
		processingTime := time.Since(startTime)
		logging.LogPerformanceMetric(label, "squadreg_response_creation", processingTime)
	}

	return &responseBuffer, nil
}

// ValidateStatusPacket validates incoming status server packets
func ValidateOrderedMessagePacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
	packetSize := len(packet)

	// Check minimum size for UserHelloMessage
	if packetSize < constants.MinHelloMessageSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too small (minimum: %d bytes)", constants.MinHelloMessageSize),
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	// Check for reasonable maximum size to prevent abuse
	if packetSize > constants.MaxBufferSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too large (maximum: %d bytes)", constants.MaxBufferSize),
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	// Validate Chromehounds header if packet is large enough
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
