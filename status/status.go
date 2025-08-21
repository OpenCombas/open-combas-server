package status

import (
	"time"
)

// quite probably matches the struct used by server
// possibly they use CHxx format for magic value, with xx being dependant on service.
//
// 31 bytes
type UserHelloMessage struct {
	ChromeHounds [4]byte //'C', 'H', 0x00, 0x00
	Xuid         [15]byte
	Unknown      [12]byte
}

// struct representing DateTime used by CH
// Flag is unknown. seen values 0x00, 0x04
//
// 8 bytes
type ServerTime struct {
	Year   uint16
	Month  uint8
	Day    uint8
	Hour   uint8
	Minute uint8
	Second uint8
	Flag   byte
}

// quite probably matches the struct used by client
// possibly they use CHxx format for magic value, with xx being dependant on service
//
// 31 bytes
type StatusHeader struct {
	ChromeHounds [4]byte
	Xuid         [15]byte
	Unknown      [12]byte
}

// Main server state strucutre used by Status server to notify client of maintenance
//
// 64 bytes
type ServerState struct {
	Header                     StatusHeader
	Unknown                    byte //possible member of header - would make it 32byte aligned
	GameSeason                 [4]byte
	ProgramVersion             [4]byte
	ServerLocalTime            ServerTime
	ServerMaintenanceStartTime ServerTime
	ServerMaintenanceEndTime   ServerTime
}

// Magic numbers for status service
var chromeHoundsHeaderValue = [4]byte{'C', 'H', '0', '0'}

// hardcoded xuid for fallback
var XuidValueHardCoded = [15]byte{
	'0', '0', '9', '0', '0', '0', '0',
	'4', 'E', 'A', '2', '5', '0', '6',
	'3'}

// unknown so far
var unknownHeaderValue = [12]byte{
	'0',
	'0', '0', '0', '0', '0', '0', '1', 0x00, 0x00, 0x00,
	0x00,
}

// exact game season value, big endian value of 3.
var gameSeasonValue = [4]byte{0x03, 0x00, 0x00, 0x00}

// version value, only this exact value works. big endian.
var programVersionValue = [4]byte{0x00, 0x00, 0x10, 0x00}

func CreateHeader(xuid [15]byte) StatusHeader {
	return StatusHeader{
		ChromeHounds: chromeHoundsHeaderValue,
		Xuid:         xuid,
		Unknown:      unknownHeaderValue,
	}
}

// Creates Servertime based on raw values, just for testing purposes
func CreateServerTimeRaw(year uint16, month uint8, day uint8, hour uint8, minute uint8, second uint8, flag byte) ServerTime {
	return ServerTime{
		Year:   year,
		Month:  month,
		Day:    day,
		Hour:   hour,
		Minute: minute,
		Second: second,
		Flag:   flag,
	}
}

// internal method for creating server time struct based on a timestamp
func createServerTime(time time.Time, flag byte) ServerTime {
	return ServerTime{

		Year:   uint16(time.Year()),
		Month:  uint8(time.Month()),
		Day:    uint8(time.Day()),
		Hour:   uint8(time.Hour()),
		Minute: uint8(time.Minute()),
		Second: uint8(time.Second()),
		Flag:   flag,
	}
}

// Create Status structure. used to respond to client via Status api
func CreateStatus(xuid [15]byte, serverTime time.Time, maintenanceStart time.Time, maintenanceEnd time.Time) ServerState {
	return ServerState{
		Header:                     CreateHeader(xuid),
		Unknown:                    0x00,
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             programVersionValue,
		ServerLocalTime:            createServerTime(serverTime, 0x04),
		ServerMaintenanceStartTime: createServerTime(maintenanceStart, 0x04),
		ServerMaintenanceEndTime:   createServerTime(maintenanceEnd, 0x00),
	}
}

// create Status with reference to server time set externally. useful for testing.
func CreateStatusRaw(xuid [15]byte, local ServerTime, maintStart ServerTime, maintEnd ServerTime) ServerState {
	return ServerState{
		Header:                     CreateHeader(xuid),
		Unknown:                    0x00,
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             programVersionValue,
		ServerLocalTime:            local,
		ServerMaintenanceStartTime: maintStart,
		ServerMaintenanceEndTime:   maintEnd,
	}
}
