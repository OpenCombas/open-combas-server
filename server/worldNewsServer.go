package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// News request body is "<gamertag>,<nation>,<page>" (RE: the 3rd field is a 1-based PAGE index, not a
// category). The client's login "news flash" fetches page 1 THEN page 2 and APPENDS both into one scroll
// list, so each page must return a DISTINCT slice -- page N = the Nth block of newsPageSize events, newest
// first. Returning identical content for two pages double-renders the login popup. A page past the end is a
// valid EMPTY terminator (count 0 is graceful client-side); we keep the full 1568B layout on every page.
const newsPageSize = 20 // 2 blocks x 10 entries

// parseNewsPage extracts the 1-based page (the 3rd comma-separated field) from a news request; default 1.
func parseNewsPage(packet []byte) int {
	if len(packet) <= constants.MinHelloMessageSize {
		return 1
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) >= 3 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil && v >= 1 {
			return v
		}
	}
	return 1
}

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
	EntityID   int16    // off 68 - the date YEAR: rendered as the middle number of the header "d0/d1/<EntityID> hh:mm"
	Code       [5]byte  // off 70 - packed date/time, raw bytes: [d0, d1, hour, minute, unused]
	_          [1]byte  // off 75 - pad to 76
}

// NewsBlock is one of the two news lists: a 24-byte header + 10 entries.
// The block header is not parsed by the wire deserializer but IS read by the news manager
// (sub_823C6EB8): header[0] is a status byte (0 = success) and header[20] is the count of valid
// records in the block. The reply is only cached/displayed when block 0 has status 0 and a
// non-zero count; the manager then sums header[20] across both blocks for the item count.
type NewsBlock struct {
	Status    byte     // off 0  - 0 = success
	_         [19]byte // off 1
	RecordNum byte     // off 20 - number of valid Entries in this block
	_         [3]byte  // off 21 - pad to 24
	Entries   [10]NewsEntry
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

// The news board must never be zero-count: the title REJECTS an empty news reply as "communication
// failed" (it gates on block-0 flag==0 && count!=0). So when the event feed is empty we serve one
// "world briefing" story. IMPORTANT: TemplateID is the PARAM ROW id into WorldSituationInfoNewsParam.bin
// (NOT a raw text id) -- row 75 = header text 14046 "War Breaks Out In Neroimus" + body 15148
// "...mutual declarations of war...", a fitting season-opener. The reset tool seeds a matching persistent
// event; this in-server fallback covers a not-yet-reset / read-error state.
const initEventRow = 75

func briefingEvent(now int64) EventRecord {
	return EventRecord{CreatedAt: now, TemplateID: initEventRow}
}

// newsEntryFromEvent renders a stored world event as a wire news item. TemplateID (short@0) selects the
// whole story (header + 2 bodies) client-side. The header date/time is composed from the event's
// timestamp across the two date fields the client reads (see the NewsEntry doc, header "d0/d1/<EntityID>
// hh:mm"): Code = [month, day, hour, minute] and EntityID = the 2-digit year (the middle "MM/DD/YY"
// number). The placeholder tokens read raw strings from C66's two 33-byte slots: Slot1 -> C66[0..32]
// (digit-1 tokens), Slot2 -> C66[33..65] (digit-2 tokens); each is capped at 32 bytes so a NUL terminator
// stays inside its slot.
func newsEntryFromEvent(ev EventRecord) NewsEntry {
	t := time.Unix(ev.CreatedAt, 0).UTC()
	e := NewsEntry{
		TemplateID: int16(ev.TemplateID),
		EntityID:   int16(t.Year() % 100), // header year slot ("MM/DD/YY")
		Code:       date(byte(int(t.Month())), byte(t.Day()), byte(t.Hour()), byte(t.Minute())),
	}
	copy(e.Text[0:32], ev.Slot1)  // slot1 (digit-1 tokens)
	copy(e.Text[33:65], ev.Slot2) // slot2 (digit-2 tokens)
	return e
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
	// generateEvents mirrors the EventServers config toggle: when false the feed serves ONLY the seeded "War
	// Breaks Out" briefing, ignoring any data-driven events in the DB (see buildNews).
	generateEvents bool
}

// buildNews serves the news feed (newest first). The request's 3rd field is a CATEGORY, NOT a page index:
// 1 = "recent (unread) news" (the login-flash popup), 2 = "all news" (the News-History screen). Confirmed by
// the client only EVER sending 1 and 2 -- true pagination would request 3,4,... once the feed exceeds the
// wire's 20-entry cap; it never does. (An earlier RE misread the two category requests as pages 1/2 and
// paginated -- that returned events[20:40] for category 2 == empty on a short feed, so News History showed
// only the briefing.) Both categories therefore return the same newest feed (the 76B wire holds at most 20
// entries, so "recent" and "all" coincide). News History reads category 2's reply, which the query-end memcpy
// (title sub_8242FBE0) copies over the cache -- so category 2 MUST carry the events, not an empty terminator.
// The briefing fallback keeps the reply non-zero-count (a zero-count reply is rejected as "communication
// failed"), matching the pre-dynamic-events static news that always returned a non-empty board.
func (s *worldNewsServer) buildNews(hi UserHelloMessage, category int) NewsState {
	var evs []EventRecord
	// With event generation disabled the board shows only the briefing; we never read the feed, so any
	// stale/generated events sitting in the DB stay hidden. Falls through to the empty-feed briefing below.
	if s.repo != nil && s.generateEvents {
		readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
		defer cancel()
		got, err := s.repo.RecentEventsPage(readCtx, 0, newsPageSize)
		if err != nil {
			logging.Warn.Printf("[%s] events read failed, using briefing: %v", s.serverConfig.Label, err)
		} else {
			evs = got
		}
	}
	if len(evs) == 0 {
		evs = []EventRecord{briefingEvent(time.Now().Unix())}
	}
	_ = category // both categories serve the same newest feed (see above)
	return newsFromEvents(hi.Xuid, hi.Order, evs)
}

func NewWorldNewsServer(listenAddress net.IP, serverConfig config.EventServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldNewsServer {
	s := &worldNewsServer{repo: repo, generateEvents: serverConfig.GenerateEvents}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig.ServerConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateWorldPacket(packet, clientAddr, serverConfig.Label)
		},
		// buildResponse (not buildPayload) so we can read the request's page field.
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			resp := s.buildNews(hi, parseNewsPage(*readBuffer))
			buf := make([]byte, constants.NewsResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
