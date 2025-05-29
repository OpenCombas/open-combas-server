package status

import (
	"time"
)

type ServerTime struct {
	year    uint16
	month   uint8
	day     uint8
	hour    uint8
	minute  uint8
	second  uint8
	unknown byte
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
	chromeHounds [2]byte
	xuid         [17]byte
	unknown      [12]byte
}

var gameSeasonValue int32 = 3
var ProgramVersionValue int32 = 3

type ServerState struct {
	header                         StatusHeader
	unknown                        byte
	gameSeason                     int32
	programVersion                 int32
	serverLocalTime                ServerTime
	serverLocalTimeFlag            byte
	serverMaintenanceStartTime     ServerTime
	serverMaintenanceStartTimeFlag byte
	serverMaintenanceEndTime       ServerTime
	serverMaintenanceEndTimeFlag   byte
	terminator                     byte
}

func createHeader() StatusHeader {
	return StatusHeader{
		chromeHounds: chromeHoundsHeaderValue,
		xuid:         xuidValue,
		unknown:      unknownHeaderValue,
	}
}

func createServerTimeRaw(year uint16, month uint8, day uint8, hour uint8, minute uint8, second uint8) ServerTime {
	return ServerTime{
		year:    year,
		month:   month,
		day:     day,
		hour:    hour,
		minute:  minute,
		second:  second,
		unknown: 0x00,
	}
}

func createServerTime(time time.Time) ServerTime {
	return ServerTime{
		year:    uint16(time.Year()),
		month:   uint8(time.Month()),
		day:     uint8(time.Day()),
		hour:    uint8(time.Hour()),
		minute:  uint8(time.Minute()),
		second:  uint8(time.Second()),
		unknown: 0x00,
	}
}

func CreateStatus(serverTime time.Time, maintenanceStart time.Time, maintenanceEnd time.Time) ServerState {
	return ServerState{
		header:                         createHeader(),
		unknown:                        0x00,
		gameSeason:                     gameSeasonValue,
		programVersion:                 ProgramVersionValue,
		serverLocalTime:                createServerTime(serverTime),
		serverLocalTimeFlag:            0x04,
		serverMaintenanceStartTime:     createServerTime(maintenanceStart),
		serverMaintenanceStartTimeFlag: 0x04,
		serverMaintenanceEndTime:       createServerTime(maintenanceEnd),
		serverMaintenanceEndTimeFlag:   0x00,
		terminator:                     0x00,
	}
}
