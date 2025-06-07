package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

// MemoryStats tracks memory usage metrics
type MemoryStats struct {
	Alloc       uint64
	TotalAlloc  uint64
	NumGC       uint32
	Mallocs     uint64
	Frees       uint64
	HeapObjects uint64
	Timestamp   time.Time
}

// PerformanceMonitor tracks server performance metrics
type PerformanceMonitor struct {
	mu                  sync.RWMutex
	startTime           time.Time
	packetsProcessed    uint64
	bytesProcessed      uint64
	totalProcessingTime time.Duration
	errorCount          uint64
	memorySnapshots     []MemoryStats
	lastMemorySnapshot  time.Time
}

var globalPerfMonitor = &PerformanceMonitor{
	startTime: time.Now(),
}

// NewPerformanceMonitor creates a new performance monitor
func NewPerformanceMonitor() *PerformanceMonitor {
	return &PerformanceMonitor{
		startTime: time.Now(),
	}
}

// GetCurrentMemoryStats returns current memory statistics
func GetCurrentMemoryStats() MemoryStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return MemoryStats{
		Alloc:       m.Alloc,
		TotalAlloc:  m.TotalAlloc,
		NumGC:       m.NumGC,
		Mallocs:     m.Mallocs,
		Frees:       m.Frees,
		HeapObjects: m.HeapObjects,
		Timestamp:   time.Now(),
	}
}

// RecordPacketProcessed records metrics for a processed packet
func (pm *PerformanceMonitor) RecordPacketProcessed(bytes int, processingTime time.Duration) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.packetsProcessed++
	pm.bytesProcessed += uint64(bytes)
	pm.totalProcessingTime += processingTime

	// Take memory snapshot every 100 packets or every 10 seconds
	now := time.Now()
	if pm.packetsProcessed%100 == 0 || now.Sub(pm.lastMemorySnapshot) > 10*time.Second {
		pm.memorySnapshots = append(pm.memorySnapshots, GetCurrentMemoryStats())
		pm.lastMemorySnapshot = now

		// Keep only last 100 snapshots to prevent memory leak
		if len(pm.memorySnapshots) > 100 {
			pm.memorySnapshots = pm.memorySnapshots[1:]
		}
	}
}

// RecordError records an error occurrence
func (pm *PerformanceMonitor) RecordError() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.errorCount++
}

// GetStats returns current performance statistics
func (pm *PerformanceMonitor) GetStats() map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	uptime := time.Since(pm.startTime)
	avgProcessingTime := time.Duration(0)
	if pm.packetsProcessed > 0 {
		avgProcessingTime = pm.totalProcessingTime / time.Duration(pm.packetsProcessed)
	}

	packetsPerSecond := float64(0)
	if uptime.Seconds() > 0 {
		packetsPerSecond = float64(pm.packetsProcessed) / uptime.Seconds()
	}

	currentMem := GetCurrentMemoryStats()

	return map[string]interface{}{
		"uptime_seconds":         uptime.Seconds(),
		"packets_processed":      pm.packetsProcessed,
		"bytes_processed":        pm.bytesProcessed,
		"packets_per_second":     packetsPerSecond,
		"avg_processing_time_ns": avgProcessingTime.Nanoseconds(),
		"error_count":            pm.errorCount,
		"current_alloc_mb":       float64(currentMem.Alloc) / 1024 / 1024,
		"total_alloc_mb":         float64(currentMem.TotalAlloc) / 1024 / 1024,
		"num_gc":                 currentMem.NumGC,
		"heap_objects":           currentMem.HeapObjects,
		"mallocs":                currentMem.Mallocs,
		"frees":                  currentMem.Frees,
	}
}

// PrintStats prints current performance statistics
func (pm *PerformanceMonitor) PrintStats() {
	stats := pm.GetStats()

	fmt.Printf("\n=== Performance Statistics ===\n")
	fmt.Printf("Uptime: %.2f seconds\n", stats["uptime_seconds"])
	fmt.Printf("Packets Processed: %d\n", stats["packets_processed"])
	fmt.Printf("Bytes Processed: %d\n", stats["bytes_processed"])
	fmt.Printf("Packets/Second: %.2f\n", stats["packets_per_second"])
	fmt.Printf("Avg Processing Time: %d ns\n", stats["avg_processing_time_ns"])
	fmt.Printf("Errors: %d\n", stats["error_count"])
	fmt.Printf("Current Memory: %.2f MB\n", stats["current_alloc_mb"])
	fmt.Printf("Total Allocated: %.2f MB\n", stats["total_alloc_mb"])
	fmt.Printf("GC Cycles: %d\n", stats["num_gc"])
	fmt.Printf("Heap Objects: %d\n", stats["heap_objects"])
	fmt.Printf("Total Mallocs: %d\n", stats["mallocs"])
	fmt.Printf("Total Frees: %d\n", stats["frees"])
	fmt.Printf("==============================\n\n")
}

// GetMemoryTrend returns memory usage trend over time
func (pm *PerformanceMonitor) GetMemoryTrend() []MemoryStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// Return a copy to prevent data races
	trend := make([]MemoryStats, len(pm.memorySnapshots))
	copy(trend, pm.memorySnapshots)
	return trend
}

// StartPeriodicReporting starts periodic performance reporting
func (pm *PerformanceMonitor) StartPeriodicReporting(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			LogPerformanceMetric("MONITOR", "periodic_stats", pm.GetStats())
		}
	}()
}

func RecordPacketProcessed(bytes int, processingTime time.Duration) {
	globalPerfMonitor.RecordPacketProcessed(bytes, processingTime)
}

func RecordError() {
	globalPerfMonitor.RecordError()
}

func PrintGlobalStats() {
	globalPerfMonitor.PrintStats()
}

func StartGlobalReporting(interval time.Duration) {
	globalPerfMonitor.StartPeriodicReporting(interval)
}
