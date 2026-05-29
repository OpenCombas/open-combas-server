package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func TestHeaderStruct(t *testing.T) {
	byteTarget := []byte{
		'C', 'H',
		'0', '0', '0', '0', '9', '0', '0', '0', '0',
		'4', 'E', 'A', '2', '5', '0', '6', '3', '0',
		'0', '0', '0', '0', '0', '0', '1',
		0x00, 0x00, 0x00, 0x00}
	strct := CreateHeader(XuidValueHardCoded, [8]byte{})

	buffer := encodeToBuffer(strct, len(byteTarget), t)
	compareBinaryBuffers(byteTarget, buffer, t)
}

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
		parts[i] = fmt.Sprintf("\\x%02x", b)
	}
	return strings.Join(parts, "")
}
