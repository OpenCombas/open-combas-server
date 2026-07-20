package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/pooling"
	"ChromehoundsStatusServer/server"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// BenchmarkConfig holds configuration for server benchmarking
type BenchmarkConfig struct {
	StatusPort          int
	EchoPort            int
	StaticMessagePort   int
	NumClients          int
	PacketsPerClient    int
	TestDurationSeconds int
	PacketSize          int
	WarmupSeconds       int
	TimeBasedTesting    bool
	PacketRateLimit     int
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

// buildBenchmarkPacket creates a valid Chromehounds test packet using UserHelloMessage
func buildBenchmarkPacket(size int) []byte {
	if size < constants.MinHelloMessageSize {
		size = constants.MinHelloMessageSize
	}

	packet := make([]byte, size)
	hello := server.UserHelloMessage{
		ChromeHounds: [4]byte{'C', 'H', 0, 0},
		Xuid:         server.XuidValueHardCoded,
	}
	if _, err := binary.Encode(packet, binary.LittleEndian, hello); err != nil {
		panic(err)
	}

	for i := constants.MinHelloMessageSize; i < size; i++ {
		packet[i] = byte(i % 256)
	}

	return packet
}

// newBenchmarkServer creates a cancellable server with minimal logging and no prometheus
func newBenchmarkServer() (net.IP, *config.LoggingConfig, config.PrometheusConfig, prometheus.Registerer) {
	listenAddr := net.ParseIP("127.0.0.1")
	appLogging := &config.LoggingConfig{
		Verbose:                     false,
		EnablePerformanceMonitoring: false,
	}
	promCfg := config.PrometheusConfig{Enabled: false}
	reg := prometheus.NewRegistry()
	return listenAddr, appLogging, promCfg, reg
}

// runBenchmarkClients launches concurrent benchmark clients against a server address
func runBenchmarkClients(
	ctx context.Context,
	cfg BenchmarkConfig,
	serverAddr string,
	results *BenchmarkResults,
	latencies chan<- LatencyMeasurement,
) {
	var clientsWg sync.WaitGroup

	for i := 0; i < cfg.NumClients; i++ {
		clientsWg.Add(1)
		go func() {
			defer clientsWg.Done()
			runBenchmarkClient(ctx, cfg, serverAddr, results, latencies)
		}()
	}

	clientsWg.Wait()
}

// runBenchmarkClient runs a single benchmark client sending packets via UDP
func runBenchmarkClient(
	ctx context.Context,
	cfg BenchmarkConfig,
	serverAddr string,
	results *BenchmarkResults,
	latencies chan<- LatencyMeasurement,
) {
	udpAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return
	}
	defer conn.Close()

	packet := buildBenchmarkPacket(cfg.PacketSize)
	responseBuffer := pooling.ReadBufferPool.Get()
	defer pooling.ReadBufferPool.Put(responseBuffer)

	var packetDelay time.Duration
	if cfg.PacketRateLimit > 0 {
		packetDelay = time.Second / time.Duration(cfg.PacketRateLimit)
	}

	packetCount := 0
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if cfg.TimeBasedTesting {
			if time.Since(startTime) >= time.Duration(cfg.TestDurationSeconds)*time.Second {
				return
			}
		} else {
			if packetCount >= cfg.PacketsPerClient {
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

		if packetDelay > 0 {
			time.Sleep(packetDelay)
		} else {
			time.Sleep(time.Microsecond * 100)
		}
	}
}

// benchmarkMemorySnapshot captures memory before a benchmark run
func benchmarkMemorySnapshot() (uint64, float64) {
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return mem.TotalAlloc, float64(mem.Alloc) / 1024 / 1024
}

// BenchmarkStatusServer tests the status server performance
func BenchmarkStatusServer(cfg BenchmarkConfig) (*BenchmarkResults, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenAddr, appLogging, promCfg, reg := newBenchmarkServer()

	var wg sync.WaitGroup
	// nil worldRepo: the benchmark exercises the wire path, and MaintenanceWindowFor falls back to the
	// silent past window without a repo.
	srv := server.NewStatusServer(
		listenAddr,
		config.StatusServerConfig{
			ServerConfig: config.ServerConfig{Label: "BENCH_STATUS", Port: cfg.StatusPort, Enabled: true, Type: config.Status},
		},
		4000, appLogging, ctx, &wg, promCfg, reg, nil,
	)
	go srv.Run()
	time.Sleep(50 * time.Millisecond)

	bufferSize := cfg.NumClients * cfg.PacketsPerClient
	if cfg.TimeBasedTesting {
		estimatedPackets := cfg.NumClients * cfg.PacketRateLimit * cfg.TestDurationSeconds
		bufferSize = estimatedPackets
	}
	if bufferSize < 1000 {
		bufferSize = 1000
	}

	latencies := make(chan LatencyMeasurement, bufferSize)

	if cfg.WarmupSeconds > 0 {
		fmt.Printf("Warming up for %d seconds...\n", cfg.WarmupSeconds)
		time.Sleep(time.Duration(cfg.WarmupSeconds) * time.Second)
	}

	allocsBefore, memBeforeMB := benchmarkMemorySnapshot()

	var results BenchmarkResults
	benchmarkStart := time.Now()
	runBenchmarkClients(ctx, cfg, fmt.Sprintf("127.0.0.1:%d", cfg.StatusPort), &results, latencies)
	results.TestDuration = time.Since(benchmarkStart)
	close(latencies)

	allocsAfter, memAfterMB := benchmarkMemorySnapshot()

	calculateLatencyStats(latencies, &results)
	calculateThroughput(&results)

	results.MemoryUsageMB = memAfterMB - memBeforeMB
	results.AllocsBefore = allocsBefore
	results.AllocsAfter = allocsAfter
	results.AllocDifference = allocsAfter - allocsBefore

	return &results, nil
}

// BenchmarkEchoServer tests echo server performance
func BenchmarkEchoServer(cfg BenchmarkConfig) (*BenchmarkResults, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenAddr, appLogging, promCfg, reg := newBenchmarkServer()

	var wg sync.WaitGroup
	srv := server.NewEchoServer(
		listenAddr,
		config.ServerConfig{Label: "BENCH_ECHO", Port: cfg.EchoPort, Enabled: true, Type: config.Echoing},
		4000, appLogging, ctx, &wg, promCfg, reg,
	)
	go srv.Run()
	time.Sleep(50 * time.Millisecond)

	bufferSize := cfg.NumClients * cfg.PacketsPerClient
	if cfg.TimeBasedTesting {
		estimatedPackets := cfg.NumClients * cfg.PacketRateLimit * cfg.TestDurationSeconds
		bufferSize = estimatedPackets
	}
	if bufferSize < 1000 {
		bufferSize = 1000
	}

	latencies := make(chan LatencyMeasurement, bufferSize)

	if cfg.WarmupSeconds > 0 {
		fmt.Printf("Warming up for %d seconds...\n", cfg.WarmupSeconds)
		time.Sleep(time.Duration(cfg.WarmupSeconds) * time.Second)
	}

	allocsBefore, memBeforeMB := benchmarkMemorySnapshot()

	var results BenchmarkResults
	benchmarkStart := time.Now()
	runBenchmarkClients(ctx, cfg, fmt.Sprintf("127.0.0.1:%d", cfg.EchoPort), &results, latencies)
	results.TestDuration = time.Since(benchmarkStart)
	close(latencies)

	allocsAfter, memAfterMB := benchmarkMemorySnapshot()

	calculateLatencyStats(latencies, &results)
	calculateThroughput(&results)

	results.MemoryUsageMB = memAfterMB - memBeforeMB
	results.AllocsBefore = allocsBefore
	results.AllocsAfter = allocsAfter
	results.AllocDifference = allocsAfter - allocsBefore

	return &results, nil
}

// BenchmarkStaticMessageServer tests static message server performance
func BenchmarkStaticMessageServer(cfg BenchmarkConfig) (*BenchmarkResults, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenAddr, appLogging, promCfg, reg := newBenchmarkServer()

	var wg sync.WaitGroup
	srvCfg := &config.StaticMessageServerConfig{
		ServerConfig: config.ServerConfig{
			Label: "BENCH_STATIC", Port: cfg.StaticMessagePort, Enabled: true,
		},
		BufferContent: "434800003030303930303030344539324241444400000000",
	}
	srv := server.NewStaticMessageServer(
		listenAddr, srvCfg, 4000, appLogging, ctx, &wg, promCfg, reg,
	)
	go srv.Run()
	time.Sleep(50 * time.Millisecond)

	bufferSize := cfg.NumClients * cfg.PacketsPerClient
	if cfg.TimeBasedTesting {
		estimatedPackets := cfg.NumClients * cfg.PacketRateLimit * cfg.TestDurationSeconds
		bufferSize = estimatedPackets
	}
	if bufferSize < 1000 {
		bufferSize = 1000
	}

	latencies := make(chan LatencyMeasurement, bufferSize)

	if cfg.WarmupSeconds > 0 {
		fmt.Printf("Warming up for %d seconds...\n", cfg.WarmupSeconds)
		time.Sleep(time.Duration(cfg.WarmupSeconds) * time.Second)
	}

	allocsBefore, memBeforeMB := benchmarkMemorySnapshot()

	var results BenchmarkResults
	benchmarkStart := time.Now()
	runBenchmarkClients(ctx, cfg, fmt.Sprintf("127.0.0.1:%d", cfg.StaticMessagePort), &results, latencies)
	results.TestDuration = time.Since(benchmarkStart)
	close(latencies)

	allocsAfter, memAfterMB := benchmarkMemorySnapshot()

	calculateLatencyStats(latencies, &results)
	calculateThroughput(&results)

	results.MemoryUsageMB = memAfterMB - memBeforeMB
	results.AllocsBefore = allocsBefore
	results.AllocsAfter = allocsAfter
	results.AllocDifference = allocsAfter - allocsBefore

	return &results, nil
}

// calculateLatencyStats computes latency statistics from measurements
func calculateLatencyStats(latencies <-chan LatencyMeasurement, results *BenchmarkResults) {
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
}

// calculateThroughput computes packets/bytes per second and success rate
func calculateThroughput(results *BenchmarkResults) {
	if results.TestDuration.Seconds() > 0 {
		results.PacketsPerSecond = float64(results.TotalPacketsReceived) / results.TestDuration.Seconds()
		results.BytesPerSecond = float64(results.TotalBytesReceived) / results.TestDuration.Seconds()
	}

	if results.TotalPacketsSent > 0 {
		results.SuccessRate = float64(results.TotalPacketsReceived) / float64(results.TotalPacketsSent) * 100
	}
}

// PrintResults prints benchmark results in a formatted way
func PrintResults(testName string, results *BenchmarkResults) {
	fmt.Printf("\n=== %s Results ===\n", testName)
	fmt.Printf("  Duration: %v (actual test time)\n", results.TestDuration)
	fmt.Printf("  Packets: %d sent, %d received (%.1f%% success)\n",
		results.TotalPacketsSent, results.TotalPacketsReceived, results.SuccessRate)
	fmt.Printf("  Throughput: %.1f packets/sec, %.1f KB/sec\n",
		results.PacketsPerSecond, results.BytesPerSecond/1024)
	fmt.Printf("  Latency: avg=%.2fms, min=%.2fms, max=%.2fms\n",
		results.AvgLatencyMs, results.MinLatencyMs, results.MaxLatencyMs)
	fmt.Printf("  Memory: %.2f MB delta, %d bytes allocated\n",
		results.MemoryUsageMB, results.AllocDifference)
}

// RunFullBenchmark runs comprehensive benchmarks
func main() {
	fmt.Println("Open Combas Server Benchmark Suite")
	fmt.Println("==================================")

	pooling.InitBufferPools(4000)

	configs := []BenchmarkConfig{
		{
			StatusPort:          1207,
			EchoPort:            1215,
			StaticMessagePort:   1241,
			NumClients:          1,
			PacketsPerClient:    1000,
			TestDurationSeconds: 10,
			PacketSize:          constants.MinHelloMessageSize,
			WarmupSeconds:       2,
			TimeBasedTesting:    false,
			PacketRateLimit:     0,
		},
		{
			StatusPort:          1207,
			EchoPort:            1215,
			StaticMessagePort:   1241,
			NumClients:          10,
			PacketsPerClient:    0,
			TestDurationSeconds: 15,
			PacketSize:          constants.MinHelloMessageSize,
			WarmupSeconds:       3,
			TimeBasedTesting:    true,
			PacketRateLimit:     100,
		},
		{
			StatusPort:          1207,
			EchoPort:            1215,
			StaticMessagePort:   1241,
			NumClients:          25,
			PacketsPerClient:    0,
			TestDurationSeconds: 20,
			PacketSize:          constants.MinHelloMessageSize,
			WarmupSeconds:       5,
			TimeBasedTesting:    true,
			PacketRateLimit:     50,
		},
	}

	for i, cfg := range configs {
		testType := "burst"
		if cfg.TimeBasedTesting {
			testType = "sustained"
		}

		fmt.Printf("\n--- Test Suite %d/%d (%s) ---\n", i+1, len(configs), testType)

		if cfg.TimeBasedTesting {
			totalRate := cfg.NumClients * cfg.PacketRateLimit
			fmt.Printf("  Clients: %d, Rate: %d pkt/sec/client (%d total/sec), Duration: %ds\n",
				cfg.NumClients, cfg.PacketRateLimit, totalRate, cfg.TestDurationSeconds)
		} else {
			fmt.Printf("  Clients: %d, Packets/Client: %d\n",
				cfg.NumClients, cfg.PacketsPerClient)
		}

		statusResults, err := BenchmarkStatusServer(cfg)
		if err != nil {
			fmt.Printf("Status server benchmark failed: %v\n", err)
		} else {
			PrintResults("Status Server", statusResults)
		}

		time.Sleep(2 * time.Second)

		echoResults, err := BenchmarkEchoServer(cfg)
		if err != nil {
			fmt.Printf("Echo server benchmark failed: %v\n", err)
		} else {
			PrintResults("Echo Server", echoResults)
		}

		time.Sleep(2 * time.Second)

		staticResults, err := BenchmarkStaticMessageServer(cfg)
		if err != nil {
			fmt.Printf("Static message server benchmark failed: %v\n", err)
		} else {
			PrintResults("Static Message Server", staticResults)
		}

		time.Sleep(3 * time.Second)
	}

	fmt.Println("\nBenchmark suite completed!")
}
