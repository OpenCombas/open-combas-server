package status

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"testing"
)

func TestDateStruct(t *testing.T) {
	byteTarget := []byte{0xE9, 0x07, 0x05, 0xf, 0x02, 0x0a, 0x00, 0x04}
	strct := createServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)

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
	strct := createHeader()

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
		'C', 'H',
		'0', '0', '0', '0', '9', '0', '0', '0', '0',
		'4', 'E', 'A', '2', '5', '0', '6', '3', '0',
		'0', '0', '0', '0', '0', '0', '1',
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x03, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x10,
		0x00,
		0xE9, 0x07, 0x05, 0xf, 0x02, 0x0a, 0x00, 0x04, 0xE9, 0x07,
		0x05, 0xf, 0x01, 0x03, 0x00, 0x04, 0xE9, 0x07, 0x05, 0xf,
		0x05, 0x03, 0x00, 0x00}

	time := createServerTimeRaw(2025, 05, 0xf, 0x02, 0x0a, 0x00, 0x04)
	maintStart := createServerTimeRaw(2025, 0x05, 0xf, 0x01, 0x03, 0x00, 0x04)
	maintEnd := createServerTimeRaw(2025, 0x05, 0xf, 0x05, 0x03, 0x00, 0x00)

	strct := CreateStatusRaw(time, maintStart, maintEnd)

	buffer := make([]byte, 64)
	if _, err := binary.Encode(buffer, binary.LittleEndian, strct); err != nil {
		panic(err)
	}
	if !bytes.Equal(byteTarget, buffer) {
		panic("Mismatch")
	}
}
