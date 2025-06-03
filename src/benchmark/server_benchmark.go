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
	fmt.Printf("🔬 Benchmarking Status Server (Port %d)\n", config.StatusPort)
	fmt.Printf("   Clients: %d, Packets/Client: %d, Duration: %ds\n",
		config.NumClients, config.PacketsPerClient, config.TestDurationSeconds)

	// Memory tracking
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.TestDurationSeconds+config.WarmupSeconds)*time.Second)
	defer cancel()

	var results BenchmarkResults
	var wg sync.WaitGroup
	latencies := make(chan LatencyMeasurement, config.NumClients*config.PacketsPerClient)

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

	for i := 0; i < config.PacketsPerClient; i++ {
		select {
		case <-ctx.Done():
			return
		default:
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

		// Small delay to prevent overwhelming
		time.Sleep(time.Microsecond * 100)
	}
}

// BenchmarkEchoServer tests echo server performance
func BenchmarkEchoServer(config BenchmarkConfig) (*BenchmarkResults, error) {
	fmt.Printf("🔬 Benchmarking Echo Server (Port %d)\n", config.EchoPort)

	// Similar implementation to status server but simpler
	// (Echo just returns what it receives)

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.TestDurationSeconds+config.WarmupSeconds)*time.Second)
	defer cancel()

	var results BenchmarkResults
	var wg sync.WaitGroup
	latencies := make(chan LatencyMeasurement, config.NumClients*config.PacketsPerClient)

	if config.WarmupSeconds > 0 {
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

	for i := 0; i < config.PacketsPerClient; i++ {
		select {
		case <-ctx.Done():
			return
		default:
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

		time.Sleep(time.Microsecond * 100)
	}
}

// PrintResults prints benchmark results in a formatted way
func PrintResults(testName string, results *BenchmarkResults) {
	fmt.Printf("\n📊 %s Results:\n", testName)
	fmt.Printf("   Duration: %v\n", results.TestDuration)
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

	// Configuration for comprehensive testing
	configs := []BenchmarkConfig{
		{
			ServerHost:          "127.0.0.1",
			StatusPort:          1207,
			EchoPort:            1215,
			NumClients:          1,
			PacketsPerClient:    1000,
			TestDurationSeconds: 10,
			PacketSize:          31,
			WarmupSeconds:       2,
		},
		{
			ServerHost:          "127.0.0.1",
			StatusPort:          1207,
			EchoPort:            1215,
			NumClients:          10,
			PacketsPerClient:    500,
			TestDurationSeconds: 15,
			PacketSize:          31,
			WarmupSeconds:       3,
		},
		{
			ServerHost:          "127.0.0.1",
			StatusPort:          1207,
			EchoPort:            1215,
			NumClients:          50,
			PacketsPerClient:    100,
			TestDurationSeconds: 20,
			PacketSize:          31,
			WarmupSeconds:       5,
		},
	}

	for i, config := range configs {
		fmt.Printf("\n🔄 Running Test Suite %d/%d\n", i+1, len(configs))

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
