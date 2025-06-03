package main

import (
	"ChromehoundsStatusServer/status"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var wg sync.WaitGroup

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	Info.Println("App started")
	var cfg = LoadConfig()
	Info.Println("Config Loaded")

	// Initialize buffer pools for performance
	InitBufferPools(cfg.BufferSize)

	// Start performance monitoring if enabled
	if cfg.EnablePerformanceMonitoring {
		StartGlobalReporting(cfg.PerfReportIntervalSec * time.Second)
		Info.Println("Performance monitoring enabled")
	}

	var address = net.ParseIP(cfg.ListeningAddress)

	go RunStatusServer(address, cfg.ServerStatusPort, "STATUS", cfg.BufferSize, ctx, &cfg)
	for _, echoCfg := range cfg.EchoingServers {
		go RunEchoingServer(address, echoCfg.Port, echoCfg.Label, cfg.BufferSize, ctx, &cfg)
	}

	// Sleep forever (or until manually stopped)
	<-ctx.Done()
	Info.Println("Shutting down")

	// Print final performance statistics before shutting down if enabled
	if cfg.EnablePerformanceMonitoring {
		PrintGlobalStats()
	}

	wg.Wait()
	Info.Println("Shut down")
}

func RunEchoingServer(listenAddress net.IP, listenPort int, label string, bufferSize int, ctx context.Context, cfg *ServerConfig) {
	wg.Add(1)
	defer wg.Done()

	conn, err := buildUDPListener(listenAddress, listenPort, label, bufferSize)
	if err != nil {
		return
	}
	defer conn.Close()

	buffer := readBufferPool.Get()
	defer readBufferPool.Put(buffer)

	// Pre-compute config flags to avoid pointer dereferencing in hot path
	enablePerfMonitoring := cfg.EnablePerformanceMonitoring
	verboseLogging := cfg.VerboseLogging

	// Pre-allocate to avoid repeated allocations
	var startTime time.Time
	var processingTime time.Duration

	for {
		select {
		case <-ctx.Done():
			if cfg.VerboseLogging {
				LogShutdown(label)
			}
			return

		default:
			if enablePerfMonitoring {
				startTime = time.Now()
			}

			n, clientAddr, err := readUDP(conn, &buffer, label)
			if err != nil {
				if !isTimeoutError(err) && enablePerfMonitoring {
					RecordError()
				}
				continue
			}

			packet := buffer[:n]

			// Validate echo packet
			if err := ValidateEchoPacket(packet, clientAddr, label); err != nil {
				if verboseLogging {
					LogPacketValidationError(label, clientAddr, err.Error(), n)
				}
				if enablePerfMonitoring {
					RecordError()
				}
				continue // Skip invalid packets
			}

			if enablePerfMonitoring {
				processingTime := time.Since(startTime)
				RecordPacketProcessed(n, processingTime)

			}
			if verboseLogging {
				LogPacketReceived(label, clientAddr, n, processingTime)

			}

			sendUDP(conn, clientAddr, &packet, label, false)
		}
	}
}

func RunStatusServer(listenAddress net.IP, listenPort int, label string, bufferSize int, ctx context.Context, cfg *ServerConfig) {
	wg.Add(1)
	defer wg.Done()

	conn, err := buildUDPListener(listenAddress, listenPort, label, bufferSize)
	if err != nil {
		return
	}
	defer conn.Close()

	readBuffer := readBufferPool.Get()
	defer readBufferPool.Put(readBuffer)

	// Pre-compute config flags to avoid pointer dereferencing in hot path
	enablePerfMonitoring := cfg.EnablePerformanceMonitoring
	verboseLogging := cfg.VerboseLogging

	// Pre-allocate to avoid repeated allocations
	var startTime time.Time
	var processingTime time.Duration

	for {
		select {
		case <-ctx.Done():
			if cfg.VerboseLogging {
				LogShutdown(label)
			}
			return

		default:
			if enablePerfMonitoring {
				startTime = time.Now()
			}

			n, clientAddr, err := readUDP(conn, &readBuffer, label)
			if err != nil {
				if !isTimeoutError(err) && enablePerfMonitoring {
					RecordError()
				}
				continue
			}

			packet := readBuffer[:n]

			// Validate status packet
			if err := ValidateStatusPacket(packet, clientAddr, label); err != nil {
				if verboseLogging {
					LogPacketValidationError(label, clientAddr, err.Error(), n)
				}
				if enablePerfMonitoring {
					RecordError()
				}
				continue // Skip invalid packets
			}

			if enablePerfMonitoring {
				processingTime := time.Since(startTime)
				RecordPacketProcessed(n, processingTime)
			}
			if verboseLogging {
				LogPacketReceived(label, clientAddr, n, processingTime)
			}

			sendBuffer, err := createStatusResponse(&packet, label, enablePerfMonitoring)
			if err != nil {
				if verboseLogging {
					Warn.Println(err)
				}
				if enablePerfMonitoring {
					RecordError()
				}
				continue
			}

			sendUDP(conn, clientAddr, sendBuffer, label, true)
		}
	}
}

func createStatusResponse(readBuffer *[]byte, label string, enablePerformanceMonitoring bool) (*[]byte, error) {
	var startTime time.Time
	if enablePerformanceMonitoring {
		startTime = time.Now()
	}

	offset := time.Minute * 10

	var helloBuffer []byte = (*readBuffer)[0:31]
	var helloStruct status.UserHelloMessage

	if _, err := binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct); err != nil {
		Warn.Printf("[%s] fallback to default xuid due to parsing error of hello header: %v\n", label, err)
		helloStruct.Xuid = status.XuidValueHardCoded
	}

	responseStruct := status.CreateStatus(helloStruct.Xuid, startTime, startTime.Add(-offset), startTime.Add(offset))

	// Use buffer pool for response
	sendBuffer := statusResponsePool.Get()
	defer statusResponsePool.Put(sendBuffer)

	// Create a copy for return since we're putting the buffer back in the pool
	responseBuffer := make([]byte, StatusResponseSize)

	if _, err := binary.Encode(responseBuffer, binary.LittleEndian, responseStruct); err != nil {
		Warn.Printf("[%s] Error populating sendbuffer: %s", label, err)
		return nil, err
	}

	if enablePerformanceMonitoring {
		processingTime := time.Since(startTime)
		LogPerformanceMetric(label, "status_response_creation", processingTime)
	}

	return &responseBuffer, nil
}

func buildUDPListener(listenAddress net.IP, listenPort int, label string, bufferSize int) (*net.UDPConn, error) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		Error.Printf("[%s] Failed to bind: %v\n", label, err)
		return nil, nil
	}

	LogServerStart(label, listenPort, bufferSize)
	return conn, nil
}

func readUDP(conn *net.UDPConn, buffer *[]byte, label string) (int, *net.UDPAddr, error) {
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, clientAddr, err := conn.ReadFromUDP(*buffer)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
		} else {
			Warn.Printf("[%s] Read error: %v\n", label, err)
		}
		return 0, nil, err
	} else if n == 0 {

		return 0, nil, fmt.Errorf("0 bytes recieved, but still recieved")
	}
	Info.Printf("[%s] Received from %s:%d -> %s\n",
		label, clientAddr.IP, clientAddr.Port, string((*buffer)[:n]))
	return n, clientAddr, nil
}

func sendUDP(conn *net.UDPConn, clientAddr *net.UDPAddr, buffer *[]byte, label string, logSend bool) error {
	bytesSent, err := conn.WriteToUDP(*buffer, clientAddr)
	if err != nil {
		Warn.Printf("[%s] send failed: %v\n", label, err)
		return err
	}

	if logSend {
		LogPacketSent(label, clientAddr, bytesSent)
	}
	return nil
}

// isTimeoutError checks if an error is a network timeout
func isTimeoutError(err error) bool {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return false
}
