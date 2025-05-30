package status_test

import (
	"ChromehoundsStatusServer/status"
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func TestDateStruct(t *testing.T) {
	byteTarget := []byte{0xE9, 0x07, 0x05, 0xf, 0x02, 0x0a, 0x00, 0x04}
	strct := status.CreateServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)

	buffer := encodeToBuffer(strct, len(byteTarget), t)
	compareBinaryBuffers(byteTarget, buffer, t)
}

func TestHeaderStruct(t *testing.T) {
	byteTarget := []byte{
		'C', 'H',
		'0', '0', '0', '0', '9', '0', '0', '0', '0',
		'4', 'E', 'A', '2', '5', '0', '6', '3', '0',
		'0', '0', '0', '0', '0', '0', '1',
		0x00, 0x00, 0x00, 0x00}
	strct := status.CreateHeader(status.XuidValueHardCoded)

	buffer := encodeToBuffer(strct, len(byteTarget), t)
	compareBinaryBuffers(byteTarget, buffer, t)
}

func TestStatusStruct(t *testing.T) {
	byteTarget := []byte{
		'C', 'H', '0', '0', '0', '0', '9', '0', '0', '0',
		'0', '4', 'E', 'A', '2', '5', '0', '6', '3', '0',
		'0', '0', '0', '0', '0', '0', '1', 0, 0, 0,
		0,
		0x00,                   // unknown
		0x03, 0x00, 0x00, 0x00, // Game Season
		0x00, 0x00, 0x10, 0x00, // program version
		0xE9, 0x07, 0x05, 0x0f, 0x02, 0x0a, 0x00, 0x04, // Server Local Time
		0xE9, 0x07, 0x05, 0x0f, 0x01, 0x03, 0x00, 0x04, // Maintenance Begins
		0xE9, 0x07, 0x05, 0x0f, 0x05, 0x03, 0x00, 0x00, // Maintenance Ends
	}

	time := status.CreateServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)
	maintStart := status.CreateServerTimeRaw(2025, 0x05, 0xf, 0x01, 0x03, 0x00, 0x04)
	maintEnd := status.CreateServerTimeRaw(2025, 0x05, 0xf, 0x05, 0x03, 0x00, 0x00)

	strct := status.CreateStatusRaw(status.XuidValueHardCoded, time, maintStart, maintEnd)

	buffer := encodeToBuffer(strct, len(byteTarget), t)
	compareBinaryBuffers(byteTarget, buffer, t)
}

// use this with structs only of fixed size
func encodeToBuffer[T any](strct T, size int, t *testing.T) []byte {
	buffer := make([]byte, size)
	if _, err := binary.Encode(buffer, binary.LittleEndian, strct); err != nil {
		t.Errorf("Encoding error: %s", err)
	}
	return buffer
}

func compareBinaryBuffers(expected []byte, result []byte, t *testing.T) {

	if len(expected) != len(result) {
		t.Errorf("Size mismatch, \nexpected: %d\nresult:%d", len(expected), len(result))
	}

	if !bytes.Equal(expected, result) {
		errorMarker := make([]string, len(expected))
		for i := range len(expected) {
			if expected[i] != result[i] {
				errorMarker[i] = "  ^^"
			} else {
				errorMarker[i] = "    "
			}
		}

		t.Errorf("Binary mismatch, \nexpected: %s\nresult:   %s\n          %s",
			encodeHex(expected),
			encodeHex(result),
			strings.Join(errorMarker, ""))
	}
}

func encodeHex(buf []byte) string {
	parts := make([]string, len(buf))
	for i, b := range buf {
		parts[i] = fmt.Sprintf("\\x%02x", b) // lowercase hex, use %02X for uppercase
	}
	return strings.Join(parts, "")
}
