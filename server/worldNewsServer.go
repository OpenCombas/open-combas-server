package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"context"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// World Situation NEWS response.
//
// Reverse-engineered from Release.xex: builder sub_823BF430 (internal message code 192) sends
// "<gamertag>,<faction 'A'/'B'/'C'>,<category>" to port 1212 (World base 1020 + 192). The news
// manager sub_823C6C48 ("@@@@@ NEWS : QUERY START/SUCCEEDED ...") issues it as an async pre-fetch
// when entering World Situation Info; the game does NOT block on a reply, which is why the area
// viewer works without this server. The reply is parsed by sub_823BDD88 into 2 blocks of 10
// records each, every record 76 bytes in wire format "S1,C66,S1,C5", records starting at
// block_base+24. Total body = 2 * (24-byte block header + 10*76) = 1568 bytes. See SERVER_ANALYSIS.md §6.
//
// Headlines are composed client-side: the headline-template id selects fragment strings from
// MenuText_Eng.fmg (ids 14000-15xxx, e.g. 14000 "Xeres Falls, Nation Dissolved"), and an entity id
// is resolved to a name via the WorldSituationInfoNews*Param.bin tables (Country id 74 -> Morskoj,
// 73 -> Tarakia, 75 -> Sal Kar). The exact roles of the two S1 ids / C66 / C5 are a best-effort
// mapping pending an in-game test; the placeholder data below is chosen to make each field's effect
// visible on the news board.

// NewsEntry is one news item (76 bytes). Wire format "S1,C66,S1,C5". Field roles confirmed in-game:
//   - TemplateID selects the whole story (title + body) from WorldSituationInfoNewsParam.bin, which
//     references MenuText_Eng.fmg fragments (e.g. id 1 -> "Ostrov Falls, Nation Dissolved" (14001),
//     2 -> "Qara Falls" (14002), 3 -> "Xeres Falls" (14000)).
//   - Text is a free-text insert for templates that contain a name placeholder; the "nation
//     dissolved" templates 1-3 have none, so it is not shown by them.
//   - EntityID renders as the middle number of the header "<Code0>/<Code1>/<EntityID> <Code2>:<Code3>".
//   - Code is a packed date/time read as RAW BYTES (not ASCII): bytes 0/1 = date pair, bytes 2/3 =
//     HH:MM, byte 4 unused.
type NewsEntry struct {
	TemplateID int16    // off 0  - story template id (WorldSituationInfoNewsParam.bin -> MenuText fragments)
	Text       [66]byte // off 2  - dynamic text insert (squad/player name etc.), NUL-terminated
	EntityID   int16    // off 68 - subject entity id; shown as the middle header number
	Code       [5]byte  // off 70 - packed date/time, raw bytes: [d0, d1, hour, minute, unused]
	_          [1]byte  // off 75 - pad to 76
}

// NewsBlock is one of the two news lists: a 24-byte header + 10 entries.
// The block header is not parsed by the wire deserializer but IS read by the news manager
// (sub_823C6EB8): header[0] is a status byte (0 = success) and header[20] is the count of valid
// records in the block. The reply is only cached/displayed when block 0 has status 0 and a
// non-zero count; the manager then sums header[20] across both blocks for the item count.
type NewsBlock struct {
	Status     byte     // off 0  - 0 = success
	_          [19]byte // off 1
	RecordNum  byte     // off 20 - number of valid Entries in this block
	_          [3]byte  // off 21 - pad to 24
	Entries    [10]NewsEntry
}

// setEntries fills the block with the given entries and the matching record count.
func (b *NewsBlock) setEntries(entries ...NewsEntry) {
	b.Status = 0
	b.RecordNum = byte(len(entries))
	for i, e := range entries {
		if i >= len(b.Entries) {
			break
		}
		b.Entries[i] = e
	}
}

// NewsState is the full reply: header(32) + 2 blocks(1568). Total = constants.NewsResponseSize (1600).
type NewsState struct {
	Header MessageHeader
	Blocks [2]NewsBlock
}

// date holds the packed Code bytes [d0, d1, hour, minute, unused] shown as "d0/d1/<EntityID> hh:mm".
func date(d0, d1, hour, minute byte) [5]byte {
	return [5]byte{d0, d1, hour, minute, 0}
}

func mkNewsEntry(templateID int16, text string, entityID int16, code [5]byte) NewsEntry {
	e := NewsEntry{TemplateID: templateID, EntityID: entityID, Code: code}
	copy(e.Text[:], text)
	return e
}

func CreateNewsState(xuid [16]byte, order [8]byte) NewsState {
	s := NewsState{Header: CreateHeader(xuid, order)}

	// The three "nation dissolved" stories, one per nation capital, with raw date/time bytes so the
	// header reads e.g. "6/28/74 12:30". setEntries also sets the block status (0) and the record
	// count header byte the news manager requires to accept the reply. Block 1 is left empty.
	s.Blocks[0].setEntries(
		mkNewsEntry(1, "", 74, date(6, 28, 12, 30)), // Ostrov Falls; entity 74 = Morskoj
		mkNewsEntry(2, "", 73, date(6, 28, 12, 31)), // Qara Falls;   entity 73 = Tarakia
		mkNewsEntry(3, "", 75, date(6, 28, 12, 32)), // Xeres Falls;  entity 75 = Sal Kar
	)

	return s
}

type worldNewsServer struct {
	*messageServer
}

func NewWorldNewsServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *worldNewsServer {
	s := &worldNewsServer{}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateWorldPacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			return CreateNewsState(hi.Xuid, hi.Order)
		},
		responseSize: constants.NewsResponseSize,
	}

	return s
}
