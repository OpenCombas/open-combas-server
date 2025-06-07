package main

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// BenchmarkConfig holds configuration for server benchmarking
type BenchmarkConfig struct {
	ServerHost          string
	StatusPort          int
	EchoPort            int
	NumClients          int
	PacketsPerClient    int
	TestDurationSeconds int
	PacketSize          int
	WarmupSeconds       int
	TimeBasedTesting    bool // New: true for time-based, false for packet-count based
	PacketRateLimit     int  // New: packets per second per client (0 = unlimited)
}

// BenchmarkResults holds the results of a benchmark run
type BenchmarkResults struct {
	TotalPacketsSent     int64
	TotalPacketsReceived int64
	TotalBytesSent       int64
	TotalBytesReceived   int64
	TestDuration         time.Duration
	PacketsPerSecond     float64
	BytesPerSecond       float64
	AvgLatencyMs         float64
	MinLatencyMs         float64
	MaxLatencyMs         float64
	SuccessRate          float64
	MemoryUsageMB        float64
	AllocsBefore         uint64
	AllocsAfter          uint64
	AllocDifference      uint64
}

// LatencyMeasurement holds individual latency measurements
type LatencyMeasurement struct {
	SendTime    time.Time
	ReceiveTime time.Time
	Latency     time.Duration
}

// ChromeHoundsTestPacket creates a valid Chromehounds test packet
func ChromeHoundsTestPacket(size int) []byte {
	if size < 31 {
		size = 31 // Minimum for UserHelloMessage
	}

	packet := make([]byte, size)
	// Set Chromehounds header
	packet[0] = 'C'
	packet[1] = 'H'
	packet[2] = '0'
	packet[3] = '0'

	// Set test XUID
	testXuid := "009000004EA25063"
	copy(packet[4:4+len(testXuid)], testXuid)

	// Fill remaining with test data
	for i := 31; i < size; i++ {
		packet[i] = byte(i % 256)
	}

	return packet
}

// BenchmarkStatusServer tests the status server performance
func BenchmarkStatusServer(config BenchmarkConfig) (*BenchmarkResults, error) {
	testType := "Burst"
	if config.TimeBasedTesting {
		testType = "Sustained"
		totalRate := config.NumClients * config.PacketRateLimit
		fmt.Printf("🔬 Benchmarking Status Server - %s Load (Port %d)\n", testType, config.StatusPort)
		fmt.Printf("   Clients: %d, Rate: %d pkt/sec/client (%d total/sec), Duration: %ds\n",
			config.NumClients, config.PacketRateLimit, totalRate, config.TestDurationSeconds)
	} else {
		fmt.Printf("🔬 Benchmarking Status Server - %s Test (Port %d)\n", testType, config.StatusPort)
		fmt.Printf("   Clients: %d, Packets/Client: %d\n",
			config.NumClients, config.PacketsPerClient)
	}

	// Memory tracking
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.TestDurationSeconds+config.WarmupSeconds+10)*time.Second) // Extra buffer
	defer cancel()

	var results BenchmarkResults
	var wg sync.WaitGroup

	// Use larger buffer for time-based testing
	bufferSize := config.NumClients * config.PacketsPerClient
	if config.TimeBasedTesting {
		// Estimate packets for time-based testing
		estimatedPackets := config.NumClients * config.PacketRateLimit * config.TestDurationSeconds
		bufferSize = estimatedPackets
	}
	if bufferSize < 1000 {
		bufferSize = 1000 // Minimum buffer size
	}

	latencies := make(chan LatencyMeasurement, bufferSize)

	// Warmup period
	if config.WarmupSeconds > 0 {
		fmt.Printf("🏃 Warming up for %d seconds...\n", config.WarmupSeconds)
		time.Sleep(time.Duration(config.WarmupSeconds) * time.Second)
	}

	benchmarkStart := time.Now()

	// Launch client goroutines
	for i := 0; i < config.NumClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			benchmarkStatusClient(ctx, config, clientID, &results, latencies)
		}(i)
	}

	wg.Wait()
	close(latencies)

	results.TestDuration = time.Since(benchmarkStart)

	// Calculate latency statistics
	var totalLatency time.Duration
	var minLatency = time.Hour // Start with a high value
	var maxLatency time.Duration
	var latencyCount int64

	for latency := range latencies {
		totalLatency += latency.Latency
		latencyCount++

		if latency.Latency < minLatency {
			minLatency = latency.Latency
		}
		if latency.Latency > maxLatency {
			maxLatency = latency.Latency
		}
	}

	if latencyCount > 0 {
		results.AvgLatencyMs = float64(totalLatency.Nanoseconds()) / float64(latencyCount) / 1e6
		results.MinLatencyMs = float64(minLatency.Nanoseconds()) / 1e6
		results.MaxLatencyMs = float64(maxLatency.Nanoseconds()) / 1e6
	}

	// Calculate rates
	if results.TestDuration.Seconds() > 0 {
		results.PacketsPerSecond = float64(results.TotalPacketsReceived) / results.TestDuration.Seconds()
		results.BytesPerSecond = float64(results.TotalBytesReceived) / results.TestDuration.Seconds()
	}

	// Success rate
	if results.TotalPacketsSent > 0 {
		results.SuccessRate = float64(results.TotalPacketsReceived) / float64(results.TotalPacketsSent) * 100
	}

	// Memory tracking
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	results.MemoryUsageMB = float64(memAfter.Alloc) / 1024 / 1024
	results.AllocsBefore = memBefore.TotalAlloc
	results.AllocsAfter = memAfter.TotalAlloc
	results.AllocDifference = memAfter.TotalAlloc - memBefore.TotalAlloc

	return &results, nil
}

// benchmarkStatusClient runs a single client for status server testing
func benchmarkStatusClient(ctx context.Context, config BenchmarkConfig, clientID int,
	results *BenchmarkResults, latencies chan<- LatencyMeasurement) {

	serverAddr, err := net.ResolveUDPAddr("udp",
		fmt.Sprintf("%s:%d", config.ServerHost, config.StatusPort))
	if err != nil {
		fmt.Printf("❌ Client %d: Failed to resolve address: %v\n", clientID, err)
		return
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		fmt.Printf("❌ Client %d: Failed to connect: %v\n", clientID, err)
		return
	}
	defer conn.Close()

	packet := ChromeHoundsTestPacket(config.PacketSize)
	responseBuffer := make([]byte, 1024)

	// Calculate rate limiting delay if needed
	var packetDelay time.Duration
	if config.PacketRateLimit > 0 {
		packetDelay = time.Second / time.Duration(config.PacketRateLimit)
	}

	packetCount := 0
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Check termination conditions
		if config.TimeBasedTesting {
			if time.Since(startTime) >= time.Duration(config.TestDurationSeconds)*time.Second {
				return
			}
		} else {
			if packetCount >= config.PacketsPerClient {
				return
			}
		}

		sendTime := time.Now()

		// Send packet
		n, err := conn.Write(packet)
		if err != nil {
			continue
		}
		atomic.AddInt64(&results.TotalPacketsSent, 1)
		atomic.AddInt64(&results.TotalBytesSent, int64(n))

		// Set read timeout
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		// Read response
		n, err = conn.Read(responseBuffer)
		receiveTime := time.Now()

		if err != nil {
			continue
		}

		atomic.AddInt64(&results.TotalPacketsReceived, 1)
		atomic.AddInt64(&results.TotalBytesReceived, int64(n))

		// Record latency
		latencies <- LatencyMeasurement{
			SendTime:    sendTime,
			ReceiveTime: receiveTime,
			Latency:     receiveTime.Sub(sendTime),
		}

		packetCount++

		// Rate limiting
		if packetDelay > 0 {
			time.Sleep(packetDelay)
		} else {
			// Small delay to prevent overwhelming
			time.Sleep(time.Microsecond * 100)
		}
	}
}

// BenchmarkEchoServer tests echo server performance
func BenchmarkEchoServer(config BenchmarkConfig) (*BenchmarkResults, error) {
	testType := "Burst"
	if config.TimeBasedTesting {
		testType = "Sustained"
		totalRate := config.NumClients * config.PacketRateLimit
		fmt.Printf("🔬 Benchmarking Echo Server - %s Load (Port %d)\n", testType, config.EchoPort)
		fmt.Printf("   Clients: %d, Rate: %d pkt/sec/client (%d total/sec), Duration: %ds\n",
			config.NumClients, config.PacketRateLimit, totalRate, config.TestDurationSeconds)
	} else {
		fmt.Printf("🔬 Benchmarking Echo Server - %s Test (Port %d)\n", testType, config.EchoPort)
		fmt.Printf("   Clients: %d, Packets/Client: %d\n",
			config.NumClients, config.PacketsPerClient)
	}

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.TestDurationSeconds+config.WarmupSeconds+10)*time.Second)
	defer cancel()

	var results BenchmarkResults
	var wg sync.WaitGroup

	// Use larger buffer for time-based testing
	bufferSize := config.NumClients * config.PacketsPerClient
	if config.TimeBasedTesting {
		estimatedPackets := config.NumClients * config.PacketRateLimit * config.TestDurationSeconds
		bufferSize = estimatedPackets
	}
	if bufferSize < 1000 {
		bufferSize = 1000
	}

	latencies := make(chan LatencyMeasurement, bufferSize)

	if config.WarmupSeconds > 0 {
		fmt.Printf("🏃 Warming up for %d seconds...\n", config.WarmupSeconds)
		time.Sleep(time.Duration(config.WarmupSeconds) * time.Second)
	}

	benchmarkStart := time.Now()

	for i := 0; i < config.NumClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			benchmarkEchoClient(ctx, config, clientID, &results, latencies)
		}(i)
	}

	wg.Wait()
	close(latencies)

	results.TestDuration = time.Since(benchmarkStart)

	// Calculate statistics (same as status server)
	var totalLatency time.Duration
	var minLatency = time.Hour
	var maxLatency time.Duration
	var latencyCount int64

	for latency := range latencies {
		totalLatency += latency.Latency
		latencyCount++
		if latency.Latency < minLatency {
			minLatency = latency.Latency
		}
		if latency.Latency > maxLatency {
			maxLatency = latency.Latency
		}
	}

	if latencyCount > 0 {
		results.AvgLatencyMs = float64(totalLatency.Nanoseconds()) / float64(latencyCount) / 1e6
		results.MinLatencyMs = float64(minLatency.Nanoseconds()) / 1e6
		results.MaxLatencyMs = float64(maxLatency.Nanoseconds()) / 1e6
	}

	if results.TestDuration.Seconds() > 0 {
		results.PacketsPerSecond = float64(results.TotalPacketsReceived) / results.TestDuration.Seconds()
		results.BytesPerSecond = float64(results.TotalBytesReceived) / results.TestDuration.Seconds()
	}

	if results.TotalPacketsSent > 0 {
		results.SuccessRate = float64(results.TotalPacketsReceived) / float64(results.TotalPacketsSent) * 100
	}

	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	results.MemoryUsageMB = float64(memAfter.Alloc) / 1024 / 1024
	results.AllocDifference = memAfter.TotalAlloc - memBefore.TotalAlloc

	return &results, nil
}

// benchmarkEchoClient runs a single client for echo server testing
func benchmarkEchoClient(ctx context.Context, config BenchmarkConfig, clientID int,
	results *BenchmarkResults, latencies chan<- LatencyMeasurement) {

	serverAddr, err := net.ResolveUDPAddr("udp",
		fmt.Sprintf("%s:%d", config.ServerHost, config.EchoPort))
	if err != nil {
		return
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return
	}
	defer conn.Close()

	testMessage := fmt.Sprintf("Echo test from client %d", clientID)
	packet := []byte(testMessage)
	responseBuffer := make([]byte, 1024)

	// Calculate rate limiting delay if needed
	var packetDelay time.Duration
	if config.PacketRateLimit > 0 {
		packetDelay = time.Second / time.Duration(config.PacketRateLimit)
	}

	packetCount := 0
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Check termination conditions
		if config.TimeBasedTesting {
			if time.Since(startTime) >= time.Duration(config.TestDurationSeconds)*time.Second {
				return
			}
		} else {
			if packetCount >= config.PacketsPerClient {
				return
			}
		}

		sendTime := time.Now()

		n, err := conn.Write(packet)
		if err != nil {
			continue
		}
		atomic.AddInt64(&results.TotalPacketsSent, 1)
		atomic.AddInt64(&results.TotalBytesSent, int64(n))

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err = conn.Read(responseBuffer)
		receiveTime := time.Now()

		if err != nil {
			continue
		}

		atomic.AddInt64(&results.TotalPacketsReceived, 1)
		atomic.AddInt64(&results.TotalBytesReceived, int64(n))

		latencies <- LatencyMeasurement{
			SendTime:    sendTime,
			ReceiveTime: receiveTime,
			Latency:     receiveTime.Sub(sendTime),
		}

		packetCount++

		// Rate limiting
		if packetDelay > 0 {
			time.Sleep(packetDelay)
		} else {
			time.Sleep(time.Microsecond * 100)
		}
	}
}

// PrintResults prints benchmark results in a formatted way
func PrintResults(testName string, results *BenchmarkResults) {
	fmt.Printf("\n📊 %s Results:\n", testName)
	fmt.Printf("   Duration: %v (actual test time)\n", results.TestDuration)
	fmt.Printf("   Packets: %d sent, %d received (%.1f%% success)\n",
		results.TotalPacketsSent, results.TotalPacketsReceived, results.SuccessRate)
	fmt.Printf("   Throughput: %.1f packets/sec, %.1f KB/sec\n",
		results.PacketsPerSecond, results.BytesPerSecond/1024)
	fmt.Printf("   Latency: avg=%.2fms, min=%.2fms, max=%.2fms\n",
		results.AvgLatencyMs, results.MinLatencyMs, results.MaxLatencyMs)
	fmt.Printf("   Memory: %.2f MB used, %d bytes allocated\n",
		results.MemoryUsageMB, results.AllocDifference)
}

// RunFullBenchmark runs comprehensive benchmarks
func main() {
	fmt.Println("🚀 Open Combas Server Benchmark Suite")
	fmt.Println("=====================================")

	// Configuration for comprehensive testing with proper time-based testing
	configs := []BenchmarkConfig{
		// Quick burst test
		{
			ServerHost:          "127.0.0.1",
			StatusPort:          1207,
			EchoPort:            1215,
			NumClients:          1,
			PacketsPerClient:    1000,
			TestDurationSeconds: 10,
			PacketSize:          31,
			WarmupSeconds:       2,
			TimeBasedTesting:    false, // Packet-count based for quick test
			PacketRateLimit:     0,     // Unlimited
		},
		// Sustained medium load
		{
			ServerHost:          "127.0.0.1",
			StatusPort:          1207,
			EchoPort:            1215,
			NumClients:          10,
			PacketsPerClient:    0, // Ignored for time-based testing
			TestDurationSeconds: 15,
			PacketSize:          31,
			WarmupSeconds:       3,
			TimeBasedTesting:    true, // Time-based testing
			PacketRateLimit:     100,  // 100 packets/sec per client = 1000 total/sec
		},
		// Sustained heavy load
		{
			ServerHost:          "127.0.0.1",
			StatusPort:          1207,
			EchoPort:            1215,
			NumClients:          25,
			PacketsPerClient:    0, // Ignored for time-based testing
			TestDurationSeconds: 20,
			PacketSize:          31,
			WarmupSeconds:       5,
			TimeBasedTesting:    true, // Time-based testing
			PacketRateLimit:     50,   // 50 packets/sec per client = 1250 total/sec
		},
	}

	for i, config := range configs {
		testType := "Burst"
		if config.TimeBasedTesting {
			testType = "Sustained"
		}

		fmt.Printf("\n🔄 Running Test Suite %d/%d (%s)\n", i+1, len(configs), testType)

		if config.TimeBasedTesting {
			totalRate := config.NumClients * config.PacketRateLimit
			fmt.Printf("   Clients: %d, Rate: %d pkt/sec/client (%d total/sec), Duration: %ds\n",
				config.NumClients, config.PacketRateLimit, totalRate, config.TestDurationSeconds)
		} else {
			fmt.Printf("   Clients: %d, Packets/Client: %d, Duration: %ds\n",
				config.NumClients, config.PacketsPerClient, config.TestDurationSeconds)
		}

		// Test Status Server
		statusResults, err := BenchmarkStatusServer(config)
		if err != nil {
			fmt.Printf("❌ Status server benchmark failed: %v\n", err)
		} else {
			PrintResults("Status Server", statusResults)
		}

		time.Sleep(2 * time.Second) // Brief pause between tests

		// Test Echo Server
		echoResults, err := BenchmarkEchoServer(config)
		if err != nil {
			fmt.Printf("❌ Echo server benchmark failed: %v\n", err)
		} else {
			PrintResults("Echo Server", echoResults)
		}

		time.Sleep(3 * time.Second) // Pause between test suites
	}

	fmt.Println("\n✅ Benchmark suite completed!")
}
