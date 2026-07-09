package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"net"
	"sync"
	"time"

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
// Headlines are composed ENTIRELY client-side: the TemplateID selects a whole fixed story (headline +
// body, WITH the capital + both nation names baked into it) from MenuText_Eng.fmg. CONFIRMED in-game
// (logs/events_and_news screenshots, 2026-07-09) -- the three "Nation Dissolved" stories are:
//   template 3 (fmg 14000) "Xeres Falls"  -> "Xeres, capital of Tarakia has fallen to Morskovian forces..."
//   template 1 (fmg 14001) "Ostrov Falls" -> "Ostrov, capital of Morskoj has fallen to Tarakian forces..."
//   template 2 (fmg 14002) "Qara Falls"   -> "Qara, capital of Sal Kar, has fallen to Tarakian forces..."
// So the "<X> has fallen to <Y>" wording is fixed template text -- the server does NOT choose the nations,
// only the TemplateID. EntityID is NOT a nation: it renders as the middle number of the header
// "<Code0>/<Code1>/<EntityID> <Code2>:<Code3>" (e.g. our EntityID 75 shows as "06/28/75"). Text is
// unused by these no-placeholder templates. See the NewsEntry field doc below.
// NOTE: these are END-GAME nation-elimination stories; CreateNewsState currently emits all three
// UNCONDITIONALLY (static placeholders), so the board shows "capital fallen / nation dissolved" even
// though the war map still has every nation alive (Xeres shown 100% Tarakia). Make it state-driven to fix.

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

// emptyNewsState is a well-formed reply with no stories: block 0 has status 0 but a zero record count,
// which the news manager accepts but renders as an empty board (no more static "nation dissolved" fakes).
func emptyNewsState(xuid [16]byte, order [8]byte) NewsState {
	return NewsState{Header: CreateHeader(xuid, order)}
}

// newsEntryFromEvent renders a stored world event as a wire news item: the client composes the whole
// headline from TemplateID; the header date is derived from the event's timestamp (UTC), EntityID is the
// header's middle number, and Text is the placeholder insert (empty for the dissolution templates).
func newsEntryFromEvent(ev EventRecord) NewsEntry {
	t := time.Unix(ev.CreatedAt, 0).UTC()
	code := date(byte(int(t.Month())), byte(t.Day()), byte(t.Hour()), byte(t.Minute()))
	return mkNewsEntry(int16(ev.TemplateID), ev.Text, int16(ev.EntityID), code)
}

// newsFromEvents packs up to 20 events (newest first) into the reply's two 10-slot blocks.
func newsFromEvents(xuid [16]byte, order [8]byte, evs []EventRecord) NewsState {
	s := NewsState{Header: CreateHeader(xuid, order)}
	entries := make([]NewsEntry, 0, len(evs))
	for _, ev := range evs {
		entries = append(entries, newsEntryFromEvent(ev))
	}
	if len(entries) > 20 {
		entries = entries[:20]
	}
	if len(entries) > 0 {
		hi := entries
		if len(hi) > 10 {
			hi = hi[:10]
		}
		s.Blocks[0].setEntries(hi...)
	}
	if len(entries) > 10 {
		s.Blocks[1].setEntries(entries[10:]...)
	}
	return s
}

type worldNewsServer struct {
	*messageServer
	repo *WorldRepository // nil when Mongo is disabled -> empty news (never the old static fakes)
}

// buildNews serves the news feed from the events collection, newest first. With no repository (Mongo off)
// or on a read error it returns an empty board rather than fabricated stories.
func (s *worldNewsServer) buildNews(hi UserHelloMessage) NewsState {
	if s.repo == nil {
		return emptyNewsState(hi.Xuid, hi.Order)
	}
	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	evs, err := s.repo.RecentEvents(readCtx, 20)
	if err != nil {
		logging.Warn.Printf("[%s] events read failed, empty news: %v", s.serverConfig.Label, err)
		return emptyNewsState(hi.Xuid, hi.Order)
	}
	return newsFromEvents(hi.Xuid, hi.Order, evs)
}

func NewWorldNewsServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldNewsServer {
	s := &worldNewsServer{repo: repo}

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
			return s.buildNews(hi)
		},
		responseSize: constants.NewsResponseSize,
	}

	return s
}
