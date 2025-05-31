package world

type WorldUserHelloMessage struct {
	ChromeHounds  [2]byte //'C', 'H'
	Delim1        [2]byte
	Xuid          [16]byte
	AccessCounter [8]byte
	Delim2        [4]byte
	Gamertag      [15]byte
}

type WorldHeader struct {
	HeaderPadding   [20]byte
	ServerResetFlag [2]byte
	FooterPadding   [38]byte
}

type FactionData struct {
	CountryCode byte
	FactionUnkn [59]byte
}
type WorldState struct {
	Header    WorldHeader
	WorldData [3]FactionData
}

// var ChromeHoundsHeaderValue = [2]byte{0x00, 0x00}
// var Delim1Value = [2]byte{0x00, 0x00}
// var Delim2Value = [4]byte{0x00, 0x00, 0x00, 0x00}
var HeaderPadding = [20]byte{0x00 * 20}
var ServerResetFlag = [2]byte{0x30, 0x32}
var FooterPadding = [38]byte{0x00 * 38}

var Padding = [3]byte{0x00 * 3}
var EndDelim = [2]byte{0x00 * 2}
var StandInTime = [4]byte{0x32, 0x00, 0x00, 0x00}
var FactionUnkn = [59]byte{0x00 * 59}

var Faction1CountryCode = 0x41
var Faction2CountryCode = 0x42
var Faction3CountryCode = 0x43

func CreateWorldHeader() WorldHeader {
	return WorldHeader{
		HeaderPadding:   HeaderPadding,
		ServerResetFlag: ServerResetFlag,
		FooterPadding:   FooterPadding,
	}
}

func CreateFactionData(countryCode byte) FactionData {
	return FactionData{
		CountryCode: countryCode,
		FactionUnkn: FactionUnkn,
	}
}

func CreateWorldState() WorldState {
	var countryCodes = [3]byte{'A', 'B', 'C'}
	var worldData [3]FactionData
	var i int = 0
	for _, countryCode := range countryCodes {
		worldData[i] = CreateFactionData(countryCode)
		i++
	}
	return WorldState{
		Header:    CreateWorldHeader(),
		WorldData: [3]FactionData(worldData),
	}
}

func CreateWorldStateRaw() WorldState {
	var countryCodes = [3]byte{'A', 'B', 'C'}
	var worldData [3]FactionData
	var i int = 0
	for _, countryCode := range countryCodes {
		worldData[i] = CreateFactionData(countryCode)
		i++
	}
	return WorldState{
		Header:    CreateWorldHeader(),
		WorldData: [3]FactionData(worldData),
	}
}
