package main

import (
	"net"
	"testing"
)

func TestValidateStatusPacket(t *testing.T) {
	clientAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	label := "TEST"

	tests := []struct {
		name        string
		packet      []byte
		expectError bool
		errorReason string
	}{
		{
			name: "Valid packet",
			packet: func() []byte {
				p := make([]byte, MinHelloMessageSize)
				copy(p[0:4], ChromeHoundsHeader[:])
				return p
			}(),
			expectError: false,
		},
		{
			name:        "Packet too small",
			packet:      make([]byte, MinHelloMessageSize-1),
			expectError: true,
			errorReason: "packet too small",
		},
		{
			name:        "Packet too large",
			packet:      make([]byte, MaxBufferSize+1),
			expectError: true,
			errorReason: "packet too large",
		},
		{
			name: "Valid Chromehounds header",
			packet: func() []byte {
				p := make([]byte, MinHelloMessageSize)
				copy(p[0:4], ChromeHoundsHeader[:])
				return p
			}(),
			expectError: false,
		},
		{
			name: "Invalid Chromehounds header",
			packet: func() []byte {
				p := make([]byte, MinHelloMessageSize)
				p[0] = 'X'
				p[1] = 'Y'
				return p
			}(),
			expectError: true,
			errorReason: "invalid Chromehounds header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStatusPacket(tt.packet, clientAddr, label)

			if tt.expectError && err == nil {
				t.Errorf("Expected error but got none")
			}

			if !tt.expectError && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}

			if tt.expectError && err != nil {
				if validationErr, ok := err.(ValidationError); ok {
					if validationErr.Size != len(tt.packet) {
						t.Errorf("Expected size %d but got %d", len(tt.packet), validationErr.Size)
					}
				}
			}
		})
	}
}

func TestValidateEchoPacket(t *testing.T) {
	clientAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	label := "TEST"

	tests := []struct {
		name        string
		packet      []byte
		expectError bool
	}{
		{
			name:        "Valid packet",
			packet:      []byte("Hello World"),
			expectError: false,
		},
		{
			name:        "Empty packet",
			packet:      []byte{},
			expectError: true,
		},
		{
			name:        "Packet too large",
			packet:      make([]byte, MaxBufferSize+1),
			expectError: true,
		},
		{
			name:        "Maximum valid size",
			packet:      make([]byte, MaxBufferSize),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEchoPacket(tt.packet, clientAddr, label)

			if tt.expectError && err == nil {
				t.Errorf("Expected error but got none")
			}

			if !tt.expectError && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
		})
	}
}
