package server

import (
	"testing"
)

func TestDateStruct(t *testing.T) {
	byteTarget := []byte{0xE9, 0x07, 0x05, 0xf, 0x02, 0x0a, 0x00, 0x04}
	strct := CreateServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)

	buffer := encodeToBuffer(strct, len(byteTarget), t)
	compareBinaryBuffers(byteTarget, buffer, t)
}

func TestStatusStruct(t *testing.T) {
	// The Status "Season ID" now reflects the server-wide season number; set it so this pins the wire
	// encoding deterministically (114 -> 0x72,0x00 LE) and proves the season flows into the reply.
	ApplySeasonNumber(114)
	defer ApplySeasonNumber(defaultSeasonNumber)

	byteTarget := []byte{
		'C', 'H', 0x00, 0x00, // magic CH\0\0
		'0', '0', '0', '9', '0', '0', '0', '0', // Xuid
		'4', 'E', 'A', '2', '5', '0', '6', '3',
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Order
		0x00, 0x00, 0x00, 0x00, // HeaderTerminator
		0x72, 0x00, 0x00, 0x00, // Game Season
		0x00, 0x00, 0x10, 0x00, // program version
		0xE9, 0x07, 0x05, 0x0f, 0x02, 0x0a, 0x00, 0x04, // Server Local Time
		0xE9, 0x07, 0x05, 0x0f, 0x01, 0x03, 0x00, 0x04, // Maintenance Begins
		0xE9, 0x07, 0x05, 0x0f, 0x05, 0x03, 0x00, 0x00, // Maintenance Ends
	}

	time := CreateServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)
	maintStart := CreateServerTimeRaw(2025, 0x05, 0xf, 0x01, 0x03, 0x00, 0x04)
	maintEnd := CreateServerTimeRaw(2025, 0x05, 0xf, 0x05, 0x03, 0x00, 0x00)

	strct := CreateStatusRaw(XuidValueHardCoded, [8]byte{}, time, maintStart, maintEnd)

	buffer := encodeToBuffer(strct, len(byteTarget), t)
	compareBinaryBuffers(byteTarget, buffer, t)
}
