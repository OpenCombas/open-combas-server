package main

import (
	"ChromehoundsStatusServer/status"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func BenchmarkCreateStatusResponseWithPool(b *testing.B) {
	// Initialize buffer pools
	InitBufferPools(4000)

	// Create test input
	testPacket := make([]byte, MinHelloMessageSize)
	copy(testPacket[0:4], ChromeHoundsHeader[:])
	copy(testPacket[4:19], status.XuidValueHardCoded[:])

	label := "BENCH"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		response, err := createStatusResponse(&testPacket, label)
		if err != nil {
			b.Fatalf("createStatusResponse failed: %v", err)
		}
		if len(*response) != StatusResponseSize {
			b.Fatalf("Expected response size %d, got %d", StatusResponseSize, len(*response))
		}
	}
}

func BenchmarkCreateStatusResponseWithoutPool(b *testing.B) {
	// Create test input
	testPacket := make([]byte, MinHelloMessageSize)
	copy(testPacket[0:4], ChromeHoundsHeader[:])
	copy(testPacket[4:19], status.XuidValueHardCoded[:])

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate old behavior without buffer pool
		currentTime := time.Now()
		offset := time.Minute * 10

		var helloBuffer []byte = testPacket[0:MinHelloMessageSize]
		var helloStruct status.UserHelloMessage

		binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct)
		responseStruct := status.CreateStatus(helloStruct.Xuid, currentTime, currentTime.Add(-offset), currentTime.Add(offset))

		// Allocate new buffer each time (old behavior)
		sendBuffer := make([]byte, StatusResponseSize)
		binary.Encode(sendBuffer, binary.LittleEndian, responseStruct)
	}
}

func BenchmarkValidateStatusPacket(b *testing.B) {
	validPacket := make([]byte, MinHelloMessageSize)
	copy(validPacket[0:4], ChromeHoundsHeader[:])

	clientAddr := mockUDPAddr()
	label := "BENCH"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ValidateStatusPacket(validPacket, clientAddr, label)
	}
}

func BenchmarkValidateEchoPacket(b *testing.B) {
	validPacket := []byte("Hello World")
	clientAddr := mockUDPAddr()
	label := "BENCH"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ValidateEchoPacket(validPacket, clientAddr, label)
	}
}

func BenchmarkBufferPoolGetPut(b *testing.B) {
	pool := NewBufferPool(4000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := pool.Get()
		pool.Put(buf)
	}
}

// Helper function for benchmarks
func mockUDPAddr() *net.UDPAddr {
	return &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 12345,
	}
}
