package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
	"bytes"
	"context"
	"net"
	"sync"
	"time"
)

func RunBufferServer(listenAddress net.IP, serverConfig *config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup) {
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

			if enablePerfMonitoring {
				processingTime := time.Since(startTime)
				profiling.RecordPacketProcessed(n, processingTime)
			}
			if verboseLogging {
				logging.LogPacketReceived(label, clientAddr, n, processingTime)
			}

			customBuffer := []byte{0x43, 0x48, 0x00, 0x00, 0x30, 0x30, 0x30, 0x39, 0x30, 0x30, 0x30, 0x30, 0x34, 0x45, 0x39, 0x32, 0x42, 0x41, 0x44, 0x44, 0x00, 0x00, 0x00, 0x00}
			zeroPadding := bytes.Repeat([]byte{0x48}, 500)
			sendBuffer := append(customBuffer, zeroPadding...)
			if err != nil {
				if verboseLogging {
					logging.Warn.Println(err)
				}
				if enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			sendUDP(conn, clientAddr, &sendBuffer, label, true)
		}
	}
}
