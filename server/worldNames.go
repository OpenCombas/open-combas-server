package server

import "strconv"

// Code-generated from WorldSituationInfoNews{Area,Field}Param.bin + MenuText_Eng.fmg (2026-07-10):
// pre-resolved region/battlefield NAMES for the WORLD_NEWS placeholder slots. The client renders raw
// strings from the entry's C66 slots, so we substitute the name server-side.

var areaNames = map[int32]string{
	1:  "Xeres",
	2:  "Ostrov",
	3:  "Qara",
	4:  "Tartat",
	5:  "Drozdovka",
	6:  "Dunaj",
	7:  "Olenya Guba",
	8:  "Mejgorye",
	9:  "Albury",
	10: "Tajin",
	11: "Baleares",
	12: "Braidwood",
	13: "Melton",
	14: "Saint Yves",
	15: "Mortlake",
	16: "Elani",
	17: "Xivera",
	18: "Gazi",
	19: "Him hime",
	20: "Berwick",
	21: "Trebizond",
	22: "Tamala",
	23: "",
	24: "",
	25: "",
}

var battlefieldNames = map[int32]string{
	1001:  "Central Xeres",
	1002:  "West Xeres",
	1003:  "East Xeres",
	1004:  "South Xeres",
	2001:  "North Ostrov",
	2002:  "Central Ostrov",
	2003:  "East Ostrov",
	2004:  "West Ostrov",
	3001:  "Central Qara",
	3002:  "North Qara",
	3003:  "South Qara",
	4001:  "Fort Ozero",
	4002:  "Fort Perimeter",
	4003:  "Fort Snowfield",
	5001:  "Zavoywko Dam 1",
	5002:  "Olensk Highlands",
	5003:  "Zavoywko Dam 2",
	6001:  "Ileckaya Base Ruins",
	6002:  "Base Ruins Perimeter",
	6003:  "Kudelyyka Valley",
	6004:  "Xikhranwi Highlands",
	7001:  "Sulimov Castle Wall",
	7002:  "Northern Walls",
	7003:  "Southern Walls",
	8001:  "East Mt. Catana",
	8002:  "West Mt. Catana",
	8003:  "North Mt. Catana",
	8004:  "South Mt. Catana",
	9001:  "Stanly Mountains",
	9002:  "Loxton Gorge",
	9003:  "Upstream of Gorge",
	9004:  "Edina Mine Ruins",
	10001: "North Village Ruin",
	10002: "South Village Ruin",
	10003: "Southworth Highway",
	10004: "Village Ruin",
	11001: "East Cale Plains",
	11002: "West Cale Plains",
	11003: "Maldon",
	11004: "Wakool",
	12001: "Cobar",
	12002: "North Bath Plains",
	12003: "South Bath Plains",
	12004: "Ebus Lake",
	13001: "Osca Industrial Zone",
	13002: "East Industrial Zone",
	13003: "West Industrial Zone",
	14001: "North Stanthorpe Bay",
	14002: "South Stanthorpe Bay",
	14003: "Bay Warehouse Dist.",
	15001: "Cecil Plains",
	15002: "East Cecil Plains",
	15003: "Kilmore Highlands",
	15004: "North Cecil Plains",
	16001: "Lake Orlovka",
	16002: "East Lake Orlovka",
	16003: "Lake Suhodol",
	17001: "Savonovo",
	17002: "Eger",
	17003: "West Salma Woods",
	17004: "East Salma Woods",
	18001: "North Cemo Oil Field",
	18002: "Dinar Plains",
	18003: "North Dinar Plains",
	18004: "South Cemo Oil Field",
	19001: "West Bayazit River",
	19002: "East Bayazit River",
	19003: "Biraal Water Plant",
	20001: "Ruins",
	20002: "Old Berri Coal Mine",
	20003: "North Cressy Desert",
	20004: "South Cressy Desert",
	21001: "Kara Bakir",
	21002: "Bas Bar",
	21003: "North Durama Desert",
	21004: "South Durama Desert",
	22001: "Pamak Mine",
	22002: "Aydin Desert",
	22003: "Bais Halayi Field",
	22004: "South Mine",
	111:   "",
	112:   "",
	113:   "",
}

func areaName(areaID int32) string               { return areaNames[areaID] }
func battlefieldName(areaID, mapID int32) string { return battlefieldNames[areaID*1000+mapID] }

// --- Name TEXT-ID maps (id-lookup rendering) ---
//
// WORLD_NEWS renders a region/battlefield name NOT from a raw string in the entry, but by looking the name
// up from a name TABLE: the |A1=/|a1= (region) and |B1 (battlefield) placeholders read their slot as a name
// reference and resolve it client-side (see areaNameSlot/battlefieldNameSlot for the exact slot encoding).
// Sourced from WorldSituationInfoNews{Area,Field}Param.bin word1 (2026-07-11); the name strings above are
// kept only for server logs.

var areaNameID = map[int32]int32{
	1:  5020,
	2:  5021,
	3:  5022,
	4:  5023,
	5:  5024,
	6:  5025,
	7:  5026,
	8:  5027,
	9:  5028,
	10: 5029,
	11: 5030,
	12: 5031,
	13: 5032,
	14: 5033,
	15: 5034,
	16: 5035,
	17: 5036,
	18: 5037,
	19: 5038,
	20: 5039,
	21: 5040,
	22: 5041,
	23: 5042,
	24: 5043,
	25: 5044,
}

var battlefieldNameID = map[int32]int32{
	1001:  5100,
	1002:  5101,
	1003:  5102,
	1004:  5103,
	2001:  5106,
	2002:  5107,
	2003:  5108,
	2004:  5109,
	3001:  5111,
	3002:  5112,
	3003:  5113,
	4001:  5115,
	4002:  5116,
	4003:  5117,
	5001:  5121,
	5002:  5122,
	5003:  5123,
	6001:  5125,
	6002:  5126,
	6003:  5127,
	6004:  5128,
	7001:  5129,
	7002:  5130,
	7003:  5131,
	8001:  5134,
	8002:  5135,
	8003:  5136,
	8004:  5137,
	9001:  5138,
	9002:  5139,
	9003:  5140,
	9004:  5141,
	10001: 5143,
	10002: 5144,
	10003: 5145,
	10004: 5146,
	11001: 5149,
	11002: 5150,
	11003: 5151,
	11004: 5152,
	12001: 5155,
	12002: 5156,
	12003: 5157,
	12004: 5158,
	13001: 5161,
	13002: 5162,
	13003: 5163,
	14001: 5166,
	14002: 5167,
	14003: 5168,
	15001: 5172,
	15002: 5173,
	15003: 5174,
	15004: 5175,
	16001: 5178,
	16002: 5179,
	16003: 5180,
	17001: 5184,
	17002: 5185,
	17003: 5186,
	17004: 5187,
	18001: 5190,
	18002: 5191,
	18003: 5192,
	18004: 5193,
	19001: 5195,
	19002: 5196,
	19003: 5197,
	20001: 5198,
	20002: 5199,
	20003: 5200,
	20004: 5201,
	21001: 5202,
	21002: 5203,
	21003: 5204,
	21004: 5205,
	22001: 5206,
	22002: 5207,
	22003: 5208,
	22004: 5209,
}

// The region/battlefield name placeholders (|A1=/|a1=/|B1) resolve a name by INDEX into the name table, not
// by absolute MenuText id: slot1 carries a 0-based index = (name text id - table base), base = the first id
// of each table (region 5020, battlefield 5100). HYPOTHESIS under test 2026-07-11 -- the absolute-id form
// rendered blank; if this 0-based-index form is also wrong the next candidates are a dense index that skips
// the %null% gaps, or a binary encoding (pending the RE handler trace).
const (
	areaNameTextBase        = 5020
	battlefieldNameTextBase = 5100
)

// areaNameSlot / battlefieldNameSlot return the ASCII-decimal name index to place in an event's slot1 for the
// region/battlefield name placeholder to resolve (empty string if unknown).
func areaNameSlot(areaID int32) string {
	if id, ok := areaNameID[areaID]; ok {
		return strconv.Itoa(int(id - areaNameTextBase))
	}
	return ""
}

func battlefieldNameSlot(areaID, mapID int32) string {
	if id, ok := battlefieldNameID[areaID*1000+mapID]; ok {
		return strconv.Itoa(int(id - battlefieldNameTextBase))
	}
	return ""
}
