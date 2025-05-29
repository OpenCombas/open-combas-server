package status_test

import (
	"ChromehoundsStatusServer/status"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"testing"
)

func TestDateStruct(t *testing.T) {
	byteTarget := []byte{0xE9, 0x07, 0x05, 0xf, 0x02, 0x0a, 0x00, 0x04}
	strct := status.CreateServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)

	var sendBuffer bytes.Buffer
	enc := gob.NewEncoder(&sendBuffer)
	if err := enc.Encode(strct); err != nil {
		panic(err)
	}

	buffer := make([]byte, 8)
	if _, err := binary.Encode(buffer, binary.LittleEndian, strct); err != nil {
		panic(err)
	}

	if !bytes.Equal(byteTarget, buffer) {
		panic("Mismatch")
	}
}

func TestHeaderStruct(t *testing.T) {
	byteTarget := []byte{
		'C', 'H',
		'0', '0', '0', '0', '9', '0', '0', '0', '0',
		'4', 'E', 'A', '2', '5', '0', '6', '3', '0',
		'0', '0', '0', '0', '0', '0', '1',
		0x00, 0x00, 0x00, 0x00}
	strct := status.CreateHeader(status.XuidValueHardCoded)

	buffer := make([]byte, 31)
	if _, err := binary.Encode(buffer, binary.LittleEndian, strct); err != nil {
		panic(err)
	}

	if !bytes.Equal(byteTarget, buffer) {
		panic("Mismatch")
	}
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

	buffer := make([]byte, 64)
	if _, err := binary.Encode(buffer, binary.LittleEndian, strct); err != nil {
		panic(err)
	}
	if !bytes.Equal(byteTarget, buffer) {
		panic("Mismatch")
	}
}
