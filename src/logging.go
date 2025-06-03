package main

import (
	"log"
	"net"
	"os"
	"time"
)

type Color string

const (
	ColorReset  Color = "\033[0m"
	ColorRed    Color = "\033[31m"
	ColorYellow Color = "\033[33m"
	ColorGreen  Color = "\033[32m"
	ColorBlue   Color = "\033[34m"
	ColorCyan   Color = "\033[36m"
)

var (
	Info    = log.New(os.Stdout, wrapInColor("INFO : ", ColorGreen), log.LstdFlags)
	Warn    = log.New(os.Stdout, wrapInColor("WARN : ", ColorYellow), log.LstdFlags)
	Error   = log.New(os.Stderr, wrapInColor("ERROR: ", ColorRed), log.LstdFlags)
	Debug   = log.New(os.Stdout, wrapInColor("DEBUG: ", ColorCyan), log.LstdFlags)
	Metrics = log.New(os.Stdout, wrapInColor("METRICS : ", ColorBlue), log.LstdFlags)
)

func wrapInColor(label string, color Color) string {
	return string(color) + label + string(ColorReset)
}

// LogServerStart logs server startup with configuration details
func LogServerStart(label string, port int, bufferSize int) {
	Info.Printf("[%s] UDP Server listening on port %d (buffer: %d bytes)",
		label, port, bufferSize)
}

// LogPacketReceived logs incoming packets with timing and size context
func LogPacketReceived(label string, clientAddr *net.UDPAddr, packetSize int, processingTime time.Duration) {
	Info.Printf("[%s] Received %d bytes from %s:%d (processed in %v)",
		label, packetSize, clientAddr.IP, clientAddr.Port, processingTime)
}

// LogPacketSent logs outgoing packets
func LogPacketSent(label string, clientAddr *net.UDPAddr, packetSize int) {
	Info.Printf("[%s] Sent %d bytes to %s:%d",
		label, packetSize, clientAddr.IP, clientAddr.Port)
}

// LogPacketValidationError logs packet validation failures
func LogPacketValidationError(label string, clientAddr *net.UDPAddr, reason string, packetSize int) {
	Warn.Printf("[%s] Packet validation failed from %s:%d - %s (size: %d)",
		label, clientAddr.IP, clientAddr.Port, reason, packetSize)
}

// LogPerformanceMetric logs performance metrics
func LogPerformanceMetric(label string, metric string, value interface{}) {
	Metrics.Printf("[%s] %s: %v", label, metric, value)
}

// LogShutdown logs graceful shutdown
func LogShutdown(label string) {
	Info.Printf("[%s] Received shutdown signal", label)
}
