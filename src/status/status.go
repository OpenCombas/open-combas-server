package status

import (
	"time"
)

type ServerTime struct {
	Year   uint16
	Month  uint8
	Day    uint8
	Hour   uint8
	Minute uint8
	Second uint8
	Flag   byte
}

var chromeHoundsHeaderValue = [2]byte{'C', 'H'}
var xuidValue = [17]byte{
	'0', '0', '0', '0', '9', '0', '0', '0',
	'0', '4', 'E', 'A', '2', '5', '0', '6', '3'}
var unknownHeaderValue = [12]byte{
	'0',
	'0', '0', '0', '0', '0', '0', '1', 0x00, 0x00, 0x00,
	0x00,
}

type StatusHeader struct {
	ChromeHounds [2]byte
	Xuid         [17]byte
	Unknown      [12]byte
}

var gameSeasonValue = [4]byte{0x00, 0x03, 0x00, 0x00}
var ProgramVersionValue = [4]byte{0x00, 0x00, 0x00, 0x10}

type ServerState struct {
	Header                     StatusHeader
	GameSeason                 [4]byte
	ProgramVersion             [4]byte
	Unknown                    byte
	ServerLocalTime            ServerTime
	ServerMaintenanceStartTime ServerTime
	ServerMaintenanceEndTime   ServerTime
}

func createHeader() StatusHeader {
	return StatusHeader{
		ChromeHounds: chromeHoundsHeaderValue,
		Xuid:         xuidValue,
		Unknown:      unknownHeaderValue,
	}
}

func createServerTimeRaw(year uint16, month uint8, day uint8, hour uint8, minute uint8, second uint8, flag byte) ServerTime {
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

func CreateStatus(serverTime time.Time, maintenanceStart time.Time, maintenanceEnd time.Time) ServerState {
	return ServerState{
		Header:                     createHeader(),
		Unknown:                    0x00,
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             ProgramVersionValue,
		ServerLocalTime:            createServerTime(serverTime, 0x04),
		ServerMaintenanceStartTime: createServerTime(maintenanceStart, 0x04),
		ServerMaintenanceEndTime:   createServerTime(maintenanceEnd, 0x00),
	}
}

func CreateStatusRaw(local ServerTime, maintStart ServerTime, maintEnd ServerTime) ServerState {
	return ServerState{
		Header:                     createHeader(),
		Unknown:                    0x00,
		GameSeason:                 gameSeasonValue,
		ProgramVersion:             ProgramVersionValue,
		ServerLocalTime:            local,
		ServerMaintenanceStartTime: maintStart,
		ServerMaintenanceEndTime:   maintEnd,
	}
}
