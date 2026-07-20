package constants

const (
	StatusResponseSize       = 64
	WorldResponseSize        = 572  // header(32) + body(540): WorldHeader(28) + 3*NationData(60) + Tail(332)
	WorldAreaResponseSize    = 764  // header(32) + body(732): WorldHeader(28) + AreaNum(4) + 25*AreaRecord(28)
	AreaInfoResponseSize     = 256  // header(32) + body(224): WorldHeader(28) + AreaID/MapCount/Flag(4) + 6*Map(32)
	MapDetailResponseSize    = 92   // header(32) + body(60): WorldHeader(28) + 1*Map(32)
	NewsResponseSize         = 1600 // header(32) + body(1568): 2 * (block hdr(24) + 10*NewsEntry(76))
	SquadRegResponseSize     = 76   // header(32) + body(44): status + TeamID(18) + UserID(18) record
	SquadJoinResponseSize    = 72   // header(32) + body(40): status + UserID(18) record; squad join (1202/1242, msgCode 182)
	SquadLoginResponseSize   = 1280 // header(32) + body(1248): TeamInfo(92) + 16*Emblem(12) + pad(4) + 20*Member(48)
	SquadAckResponseSize     = 34   // header(32) + 2-byte status body; squad lock (1211) / config upload (1205)
	BattleReportResponseSize = 39   // header(32) + 7-byte CSV status "1,0,0,0"; mission result (1214/msgCode 194)
	// header(32) + status(1) + pad(1) + 10*SquadHistoryRecord(134). The PAD is required: the client takes
	// the record base as `status + 2` (sub_821C8F20: `a1[46] = a2 + 2`, terminator scan at `a2[134*i + 2]`),
	// not status + 1. Packing records immediately after the status byte shifts every field by one.
	SquadHistoryResponseSize = 1374
	SquadHistoryRecordBase   = 2   // bytes from the status byte to record 0; see above
	SquadHistoryRecordSize   = 134 // "S2,C65,C65": type/year shorts + 2x char[65]; type-0 record terminates
	SquadHistoryMaxRecords   = 10   // client reads at most 10 dated events per squad
	OrderedMessageSize       = 532
	MinHelloMessageSize      = 32
	MaxBufferSize            = 65535
)
