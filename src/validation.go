package main

import (
	"fmt"
	"net"
)

// ValidationError represents a packet validation error
type ValidationError struct {
	Reason string
	Size   int
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("validation failed: %s (packet size: %d)", e.Reason, e.Size)
}

// ValidateStatusPacket validates incoming status server packets
func ValidateStatusPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
	packetSize := len(packet)

	// Check minimum size for UserHelloMessage
	if packetSize < MinHelloMessageSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too small (minimum: %d bytes)", MinHelloMessageSize),
			Size:   packetSize,
		}
		LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	// Check for reasonable maximum size to prevent abuse
	if packetSize > MaxBufferSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too large (maximum: %d bytes)", MaxBufferSize),
			Size:   packetSize,
		}
		LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	// Validate Chromehounds header if packet is large enough
	expectedHeader := ChromeHoundsHeader
	if packet[0] != expectedHeader[0] || packet[1] != expectedHeader[1] {
		err := ValidationError{
			Reason: "invalid Chromehounds header",
			Size:   packetSize,
		}
		LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	return nil
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
		LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	if packetSize > MaxBufferSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too large (maximum: %d bytes)", MaxBufferSize),
			Size:   packetSize,
		}
		LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	return nil
}
