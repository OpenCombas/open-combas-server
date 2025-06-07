package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Color codes for output
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorCyan   = "\033[36m"
)

// Config holds benchmark configuration
type Config struct {
	Mode         string
	OutputFile   string
	ServerBinary string
	ProjectDir   string
	Quick        bool
	Standard     bool
	Extensive    bool
}

// BenchmarkRunner manages the entire benchmark process
type BenchmarkRunner struct {
	config       Config
	serverCmd    *exec.Cmd
	serverCancel context.CancelFunc
	results      []string
	startTime    time.Time
}

// NewBenchmarkRunner creates a new benchmark runner
func NewBenchmarkRunner(config Config) *BenchmarkRunner {
	return &BenchmarkRunner{
		config:    config,
		results:   make([]string, 0),
		startTime: time.Now(),
	}
}

// Color output functions
func colorPrint(color, format string, args ...interface{}) {
	fmt.Printf(color+format+ColorReset+"\n", args...)
}

func printHeader(title string) {
	colorPrint(ColorCyan, "\n========================================")
	colorPrint(ColorCyan, "  %s", title)
	colorPrint(ColorCyan, "========================================")
}

func printSuccess(format string, args ...interface{}) {
	colorPrint(ColorGreen, "✅ "+format, args...)
}

func printError(format string, args ...interface{}) {
	colorPrint(ColorRed, "❌ "+format, args...)
}

func printWarning(format string, args ...interface{}) {
	colorPrint(ColorYellow, "⚠️  "+format, args...)
}

func printInfo(format string, args ...interface{}) {
	colorPrint(ColorBlue, "ℹ️  "+format, args...)
}

// System information
func (br *BenchmarkRunner) printSystemInfo() {
	printHeader("System Information")

	printInfo("OS: %s %s", runtime.GOOS, runtime.GOARCH)
	printInfo("Go Version: %s", runtime.Version())
	printInfo("CPU Cores: %d", runtime.NumCPU())

	// Get memory info
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	printInfo("Available Memory: %d MB", m.Sys/1024/1024)
}

// Check if UDP port is responding (specific to our server)
func isPortOpen(host string, port int) bool {
	// For port 1207 (status server), try to send a test packet
	if port == 1207 {
		return isStatusServerReady(host, port)
	}

	// For other ports, try a simple UDP connection
	timeout := time.Second * 3
	conn, err := net.DialTimeout("udp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

// Check if status server is ready by sending a test packet
func isStatusServerReady(host string, port int) bool {
	// Create a simple test packet (Chromehounds format)
	testPacket := make([]byte, 31)
	testPacket[0] = 'C'
	testPacket[1] = 'H'
	testPacket[2] = '0'
	testPacket[3] = '0'

	// Test XUID
	copy(testPacket[4:], "009000004EA25063")

	// Try to connect and send packet
	serverAddr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("udp", serverAddr, time.Second*2)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Set timeouts
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	conn.SetReadDeadline(time.Now().Add(time.Second))

	// Send test packet
	_, err = conn.Write(testPacket)
	if err != nil {
		return false
	}

	// Try to read response (should get 64 bytes back)
	response := make([]byte, 128)
	n, err := conn.Read(response)
	if err != nil {
		return false
	}

	// Check if we got a reasonable response (status response should be 64 bytes)
	return n >= 32 // Allow some flexibility in response size
}

// Wait for server to be ready
func (br *BenchmarkRunner) waitForServer(timeout time.Duration) bool {
	printInfo("Waiting for server to be ready...")
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if isPortOpen("127.0.0.1", 1207) {
			printSuccess("Server is ready!")
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}

	printError("Server failed to start within %v", timeout)
	return false
}

// Check if port appears to be in use (for preflight checks)
func isPortInUse(host string, port int) bool {
	// Try both TCP and UDP to see if anything is listening

	// Check TCP first
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Millisecond*500); err == nil {
		conn.Close()
		return true
	}

	// Check UDP - this is trickier, just try to bind to the port briefly
	if addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port))); err == nil {
		if conn, err := net.ListenUDP("udp", addr); err == nil {
			conn.Close()
			return false // We could bind, so it's free
		} else {
			return true // Couldn't bind, probably in use
		}
	}

	return false
}

// Run preflight checks
func (br *BenchmarkRunner) runPreflightChecks() bool {
	printHeader("Preflight Checks")

	// Check if Go is available
	if _, err := exec.LookPath("go"); err != nil {
		printError("Go is not installed or not in PATH")
		return false
	}
	printSuccess("Go is available")

	// Check if we're in the right directory
	if _, err := os.Stat(filepath.Join(br.config.ProjectDir, "go.mod")); err != nil {
		printError("go.mod not found in project directory: %s", br.config.ProjectDir)
		return false
	}
	printSuccess("Found go.mod in project directory")

	// Check port availability
	ports := []int{1207, 1215, 1255}
	for _, port := range ports {
		if isPortInUse("127.0.0.1", port) {
			printWarning("Port %d appears to be in use", port)
		} else {
			printSuccess("Port %d is available", port)
		}
	}

	return true
}

// Build the server
func (br *BenchmarkRunner) buildServer() (string, error) {
	printHeader("Building Server")

	if br.config.ServerBinary != "" {
		if _, err := os.Stat(br.config.ServerBinary); err != nil {
			return "", fmt.Errorf("server binary not found: %s", br.config.ServerBinary)
		}
		printSuccess("Using provided server binary: %s", br.config.ServerBinary)
		return br.config.ServerBinary, nil
	}

	printInfo("Building Open Combas server...")

	// Determine output name based on OS
	outputName := "open-combas-server"
	if runtime.GOOS == "windows" {
		outputName += ".exe"
	}

	outputPath := filepath.Join(br.config.ProjectDir, outputName)

	// Clean existing build
	if _, err := os.Stat(outputPath); err == nil {
		os.Remove(outputPath)
	}

	// Build command
	cmd := exec.Command("go", "build", "-o", outputPath, ".")
	cmd.Dir = br.config.ProjectDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		printError("Failed to build server: %v", err)
		fmt.Printf("Build output: %s\n", string(output))
		return "", err
	}

	printSuccess("Server built successfully: %s", outputPath)
	return outputPath, nil
}

// Build benchmark tool
func (br *BenchmarkRunner) buildBenchmarkTool() (string, error) {
	printHeader("Building Benchmark Tool")

	benchmarkDir := filepath.Join(br.config.ProjectDir, "benchmark")
	if _, err := os.Stat(benchmarkDir); err != nil {
		printError("Benchmark directory not found: %s", benchmarkDir)
		return "", err
	}

	printInfo("Building benchmark tool...")

	toolName := "benchmark-tool"
	if runtime.GOOS == "windows" {
		toolName += ".exe"
	}

	toolPath := filepath.Join(br.config.ProjectDir, toolName)

	// Build command
	cmd := exec.Command("go", "build", "-o", toolPath, ".")
	cmd.Dir = benchmarkDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		printError("Failed to build benchmark tool: %v", err)
		fmt.Printf("Build output: %s\n", string(output))
		return "", err
	}

	printSuccess("Benchmark tool built successfully: %s", toolPath)
	return toolPath, nil
}

// Start the server
func (br *BenchmarkRunner) startServer(serverPath string) error {
	printHeader("Starting Server")

	// Kill any existing processes
	br.killExistingServers()

	printInfo("Starting server: %s", serverPath)

	// Create context for server management
	ctx, cancel := context.WithCancel(context.Background())
	br.serverCancel = cancel

	// Start server
	br.serverCmd = exec.CommandContext(ctx, serverPath)
	br.serverCmd.Dir = br.config.ProjectDir

	// Capture output for debugging
	br.serverCmd.Stdout = os.Stdout
	br.serverCmd.Stderr = os.Stderr

	if err := br.serverCmd.Start(); err != nil {
		printError("Failed to start server: %v", err)
		return err
	}

	printSuccess("Server started (PID: %d)", br.serverCmd.Process.Pid)

	// Wait for server to be ready
	if !br.waitForServer(30 * time.Second) {
		br.stopServer()
		return fmt.Errorf("server failed to become ready")
	}

	return nil
}

// Kill existing server processes
func (br *BenchmarkRunner) killExistingServers() {
	// This is a simple approach - in production you'd want more sophisticated process management
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/F", "/IM", "open-combas-server.exe").Run()
	} else {
		exec.Command("pkill", "-f", "open-combas-server").Run()
	}
}

// Stop the server
func (br *BenchmarkRunner) stopServer() {
	printHeader("Stopping Server")

	if br.serverCancel != nil {
		br.serverCancel()
	}

	if br.serverCmd != nil && br.serverCmd.Process != nil {
		printInfo("Stopping server (PID: %d)", br.serverCmd.Process.Pid)

		// Try graceful shutdown first
		br.serverCmd.Process.Signal(os.Interrupt)

		// Wait a bit, then force kill if needed
		done := make(chan error, 1)
		go func() {
			done <- br.serverCmd.Wait()
		}()

		select {
		case <-done:
			printSuccess("Server stopped gracefully")
		case <-time.After(5 * time.Second):
			printWarning("Force killing server")
			br.serverCmd.Process.Kill()
		}
	}

	// Clean up any remaining processes
	br.killExistingServers()
}

// Run unit tests
func (br *BenchmarkRunner) runUnitTests() bool {
	printHeader("Running Unit Tests")

	printInfo("Running Go unit tests...")

	cmd := exec.Command("go", "test", "-v", "./...")
	cmd.Dir = br.config.ProjectDir

	output, err := cmd.CombinedOutput()
	fmt.Printf("%s\n", string(output))

	if err != nil {
		printError("Some unit tests failed")
		return false
	}

	printSuccess("All unit tests passed")
	return true
}

// Run Go benchmark tests
func (br *BenchmarkRunner) runGoBenchmarks() bool {
	printHeader("Running Go Benchmark Tests")

	printInfo("Running Go benchmark tests...")

	cmd := exec.Command("go", "test", "-bench=.", "-benchmem", "./...")
	cmd.Dir = br.config.ProjectDir

	output, err := cmd.CombinedOutput()
	fmt.Printf("%s\n", string(output))

	if err != nil {
		printWarning("Benchmark tests had issues")
		return false
	}

	printSuccess("Benchmark tests completed")
	return true
}

// Run server benchmarks
func (br *BenchmarkRunner) runServerBenchmarks(toolPath string) bool {
	printHeader(fmt.Sprintf("Running Server Benchmarks (%s mode)", br.config.Mode))

	if _, err := os.Stat(toolPath); err != nil {
		printError("Benchmark tool not found: %s", toolPath)
		return false
	}

	printInfo("Starting benchmark suite...")

	cmd := exec.Command(toolPath)
	cmd.Dir = br.config.ProjectDir

	output, err := cmd.CombinedOutput()

	if err != nil {
		printError("Benchmarks failed: %v", err)
		fmt.Printf("Output: %s\n", string(output))
		return false
	}

	printSuccess("Benchmarks completed successfully")
	fmt.Printf("\nBenchmark Output:\n%s\n", string(output))

	// Store results for report
	br.results = append(br.results, string(output))

	return true
}

// Generate report
func (br *BenchmarkRunner) generateReport() error {
	printHeader("Generating Report")

	duration := time.Since(br.startTime)

	report := fmt.Sprintf(`Open Combas Server Benchmark Report
Generated: %s
Duration: %v
Mode: %s

System Information:
OS: %s %s
Go Version: %s
CPU Cores: %d

Benchmark Results:
%s

Test completed successfully.
`,
		time.Now().Format("2006-01-02 15:04:05"),
		duration,
		br.config.Mode,
		runtime.GOOS, runtime.GOARCH,
		runtime.Version(),
		runtime.NumCPU(),
		strings.Join(br.results, "\n\n"),
	)

	if br.config.OutputFile != "" {
		if err := os.WriteFile(br.config.OutputFile, []byte(report), 0644); err != nil {
			printError("Failed to save report: %v", err)
			return err
		}
		printSuccess("Report saved to: %s", br.config.OutputFile)
	} else {
		fmt.Printf("\n%s\n", report)
	}

	return nil
}

// Cleanup
func (br *BenchmarkRunner) cleanup() {
	printHeader("Cleanup")

	br.stopServer()

	// Remove build artifacts
	artifacts := []string{"benchmark-tool", "benchmark-tool.exe"}
	for _, artifact := range artifacts {
		path := filepath.Join(br.config.ProjectDir, artifact)
		if _, err := os.Stat(path); err == nil {
			os.Remove(path)
			printInfo("Removed %s", artifact)
		}
	}

	printSuccess("Cleanup completed")
}

// Run the complete benchmark suite
func (br *BenchmarkRunner) Run() error {
	defer br.cleanup()

	printHeader("Open Combas Server Benchmark Suite")
	printInfo("Mode: %s", br.config.Mode)
	printInfo("Start Time: %s", br.startTime.Format("2006-01-02 15:04:05"))

	// System info and preflight checks
	br.printSystemInfo()

	if !br.runPreflightChecks() {
		return fmt.Errorf("preflight checks failed")
	}

	// Build components
	serverPath, err := br.buildServer()
	if err != nil {
		return fmt.Errorf("failed to build server: %v", err)
	}

	toolPath, err := br.buildBenchmarkTool()
	if err != nil {
		return fmt.Errorf("failed to build benchmark tool: %v", err)
	}

	// Run tests based on mode
	if !br.config.Quick {
		br.runUnitTests()
		br.runGoBenchmarks()
	}

	// Start server and run benchmarks
	if err := br.startServer(serverPath); err != nil {
		return fmt.Errorf("failed to start server: %v", err)
	}

	// Give server time to fully initialize
	time.Sleep(2 * time.Second)

	if !br.runServerBenchmarks(toolPath) {
		printWarning("Server benchmarks failed, but continuing...")
	}

	// Generate report
	if err := br.generateReport(); err != nil {
		return fmt.Errorf("failed to generate report: %v", err)
	}

	printSuccess("Benchmark suite completed successfully!")
	return nil
}

func main() {
	var config Config

	// Parse command line flags
	flag.StringVar(&config.Mode, "mode", "standard", "Benchmark mode (quick, standard, extensive)")
	flag.StringVar(&config.OutputFile, "output", "", "Output file for results")
	flag.StringVar(&config.ServerBinary, "server", "", "Path to server binary (optional)")
	flag.Parse()

	// Set mode flags
	config.Quick = config.Mode == "quick"
	config.Standard = config.Mode == "standard"
	config.Extensive = config.Mode == "extensive"

	// Determine project directory
	if wd, err := os.Getwd(); err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	} else {
		config.ProjectDir = wd
	}

	// Create and run benchmark
	runner := NewBenchmarkRunner(config)

	if err := runner.Run(); err != nil {
		printError("Benchmark suite failed: %v", err)
		os.Exit(1)
	}
}
