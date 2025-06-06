package team

type TeamUserHelloMessage struct {
	ChromeHounds    [2]byte
	Delim1          [2]byte
	Xuid            [16]byte
	AccessCounter   [8]byte
	Delim2          [4]byte
	GamertagAndTeam [33]byte
}

type TeamData struct {
	HeaderPadding [620]byte
}

var HeaderPadding = [620]byte{0x00 * 620}

func CreateTeamData() TeamData {
	return TeamData{
		HeaderPadding: HeaderPadding,
	}
}

func CreateTeamStateRaw() TeamData {
	return TeamData{
		HeaderPadding: HeaderPadding,
	}
}
