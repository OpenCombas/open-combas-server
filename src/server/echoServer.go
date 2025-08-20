package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

func RunEchoingServer(listenAddress net.IP, serverConfig *config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	// Pre-compute config flags to avoid pointer dereferencing in hot path
	enablePerfMonitoring := loggingConfig.EnablePerformanceMonitoring
	verboseLogging := loggingConfig.Verbose
	label := serverConfig.Label

	conn, err := buildUDPListener(listenAddress, serverConfig.Port, label, bufferSize)
	if err != nil {
		return
	}
	defer conn.Close()

	buffer := pooling.ReadBufferPool.Get()
	defer pooling.ReadBufferPool.Put(buffer)

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

			n, clientAddr, err := readUDP(conn, &buffer, label)
			if err != nil {
				if !isTimeoutError(err) && enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			packet := buffer[:n]

			// Validate echo packet
			if err := ValidateEchoPacket(packet, clientAddr, label); err != nil {
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

			sendUDP(conn, clientAddr, &packet, label, false)
		}
	}
}

// ValidateEchoPacket validates incoming echo server packets
func ValidateEchoPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
	packetSize := len(packet)

	// Basic size validation for echo packets
	if packetSize == 0 {
		err := ValidationError{
			Reason: "empty packet",
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
