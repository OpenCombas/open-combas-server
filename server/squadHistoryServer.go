package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Squad history ("History" screen) response.
//
// Reverse-engineered by the client instance from Release.xex (xex_re.md "SQUAD HISTORY server"): the
// CH_SquadHistoryMenu dialog (state machine sub_821C97E8) fetches a squad's dated event feed over the
// SAME combas-query connection as WORLD_NEWS. Builder sub_823BF380; the request body is "<gamertag>,<squad
// id>" (identical to the squad-login body, so we reuse parseSquadLogin). The reply is read into dialog buf
// a1+212 by sub_821C8D10.
//
// WIRE (confident): [1 status byte: 0 = OK] + up to 10 records x 134 B. Each record is "S2, C65, C65"
// deserialized big-endian by sub_823B55E8: two u16 shorts (TYPE, YEAR) then two char[65] blocks. The UI
// reads month/day/hour/min/sec as the first 5 bytes of block-1, then per-type content. A record whose TYPE
// (u16 @0) == 0 terminates the list, so a zero-filled tail is a natural terminator and an empty history is
// simply status 0 + a lone type-0 record.
//
//	off +0   u16 TYPE   (2..7; 0 = terminator)          off +2   u16 YEAR
//	off +4   u8 month   +5 day  +6 hour  +7 min  +8 sec
//	off +9   content A (block-1): type 2/3 = UTF-8 member name; type 4/5 = ASCII grade index (-> FMG 5700+idx)
//	off +105 ASCII id2  off +108 ASCII id1 (block-2): type 6/7 = battle area/map ids -> map-name lookup
//
// msgCode = 199 (debug port 1219 / retail 1259). The earlier "199 = squad DISBAND" table entry was a
// mislabel: the combas builder for 199 is sub_823BF380 with parser/dump sub_823BDBF0 -- the exact pair
// the SQUAD HISTORY RE cites, both inside the combas builder range 0x823BE880-0x823BF998. The "disband"
// addresses (0x8234EAC0/0x82192648) sit outside that range and read the squad global (dword_8290F5E0+392):
// disband is a LOCAL UI leader action, not a combas message (server-side a squad disbands via the
// 183-withdraw last-member path). See server.md S-Q22 (resolved).
//
// FRAMING (resolved 2026-07-20, sub_821C8F20): records start at status + 2, and the 10-record cap is real
// -- the scan is `do { ... } while (i < 0xA)`, so no 11th terminator slot is needed. That closes the
// full-10 half of S-Q23. Confirmed in-game: a type-4 event rendered "The squad grade has gone up to
// Rookie+" with a correct 07/20/26 date.
//
// STILL UNCONFIRMED: the +105/+108 area-vs-map argument order for types 6/7 (battle events). Those two
// ids feed sub_82193EE0(global, *(+105), *(+108)) and both are plausible map-name lookup args, so a swap
// would surface as a WRONG map name rather than a blank or broken row -- it cannot be caught by framing
// checks, only by comparing a rendered battle event against the area it was actually fought in.

// historyRecord packs one event into its 134-byte wire record.
//
// ENDIANNESS: LITTLE-endian, CONFIRMED in-game 2026-07-20. sub_823B55E8 byte-SWAPS each element in place
// after aligning it, and the console is big-endian, so a value that should read as N goes on the wire
// byte-reversed -- the same convention as every other combas reply. The C65 string blocks are unaffected;
// 1-byte elements are not swapped.
//
// How it was confirmed, since it was asserted before it was tested: with the record framing fixed (see
// buildHistoryResponse) the History screen rendered "07/20/26 12:31:38". A big-endian year would have read
// 0xEA07 = 59911 and displayed 11. The date fields are single bytes and prove nothing either way, so the
// YEAR is the only field in this record that distinguishes the two encodings.
//
// This file previously wrote both u16s big-endian AND its tests asserted big-endian, so they agreed with
// each other and disagreed with the console -- which is why it survived. That was a real bug, but it was
// NOT the cause of the "bars with no content" screen; the one-byte framing error below was.
func historyRecord(ev SquadHistoryEvent) [constants.SquadHistoryRecordSize]byte {
	var rec [constants.SquadHistoryRecordSize]byte
	binary.LittleEndian.PutUint16(rec[0:2], uint16(ev.Type))

	t := time.Unix(ev.CreatedAt, 0).UTC()
	binary.LittleEndian.PutUint16(rec[2:4], uint16(t.Year()))
	rec[4] = byte(t.Month())
	rec[5] = byte(t.Day())
	rec[6] = byte(t.Hour())
	rec[7] = byte(t.Minute())
	rec[8] = byte(t.Second())

	switch ev.Type {
	case historyTypeJoined, historyTypeLeft:
		putHistoryString(rec[:], 9, 68, ev.Name) // content A = member name (block-1, after the 5 date bytes)
	case historyTypeGradeUp, historyTypeGradeDown:
		putHistoryString(rec[:], 9, 68, strconv.Itoa(int(ev.GradeIdx))) // ASCII grade index -> FMG 5700+idx
	case historyTypeInvading, historyTypeDefense:
		// Battle location: two ASCII-decimal ids in block-2. id1 @+108 / id2 @+105 feed the map-name
		// lookup (sub_82193EE0). Arg order (which is area vs map) is unconfirmed -- see S-Q23.
		putHistoryString(rec[:], 105, 107, strconv.Itoa(int(ev.MapID)))  // id2 (arg2)
		putHistoryString(rec[:], 108, 133, strconv.Itoa(int(ev.AreaID))) // id1 (arg1)
	}
	return rec
}

// putHistoryString copies s into rec[start:] as a NUL-terminated ASCII/UTF-8 field, never writing past
// end (inclusive) so it stays inside its C65 block and always leaves room for the terminator.
func putHistoryString(rec []byte, start, end int, s string) {
	max := end - start // reserve rec[end] for the NUL terminator
	if max < 0 {
		return
	}
	n := copy(rec[start:start+max], s)
	rec[start+n] = 0
}

// buildHistoryResponse assembles the full reply: header(32) + status(1) + 10 record slots. Events are
// packed newest-first into the leading slots; unused trailing slots stay zero, i.e. type-0 terminators.
func buildHistoryResponse(hi UserHelloMessage, evs []SquadHistoryEvent) []byte {
	buf := make([]byte, constants.SquadHistoryResponseSize)
	// Header first (all byte-array fields, so little-endian encoding preserves their order).
	if _, err := binary.Encode(buf[:constants.MinHelloMessageSize], binary.LittleEndian, CreateHeader(hi.Xuid, hi.Order)); err != nil {
		// A header encode can't realistically fail for fixed byte arrays; if it did, the zeroed buffer is
		// still a well-formed empty reply.
		return buf
	}
	buf[constants.MinHelloMessageSize] = 0 // status: 0 = OK

	// Records start TWO bytes after the status byte, not one. The client sets its record base to
	// `status + 2` and scans terminators at `status + 2 + 134*i` (sub_821C8F20). Using +1 shifted every
	// record by a byte, which is what produced the "bars with no content" History screen: the type u16
	// straddled the year field and read non-zero (so a row was drawn) but matched no content case, and the
	// date rendered as 20/20/99 31:38:50 -- month<-day, day<-hour, hour<-minute, minute<-second, and the
	// "seconds" was the ASCII digit of the grade index at +9.
	recBase := constants.MinHelloMessageSize + constants.SquadHistoryRecordBase
	for i, ev := range evs {
		if i >= constants.SquadHistoryMaxRecords {
			break
		}
		rec := historyRecord(ev)
		copy(buf[recBase+i*constants.SquadHistoryRecordSize:], rec[:])
	}
	return buf
}

type squadHistoryServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> empty history (status 0 + terminator)
}

// buildHistory serves a squad's newest events. An empty/absent squad id or an unknown squad yields an
// empty (graceful) history; a read error also falls back to empty so the History screen never wedges on a
// missing reply.
func (s *squadHistoryServer) buildHistory(hi UserHelloMessage, packet []byte) []byte {
	if s.repo == nil {
		return buildHistoryResponse(hi, nil)
	}
	_, teamID := parseSquadLogin(packet) // request body is "<gamertag>,<squad id>", same as squad login
	if teamIDIsEmpty(teamID) {
		return buildHistoryResponse(hi, nil)
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	evs, err := s.repo.RecentSquadHistory(readCtx, teamID, constants.SquadHistoryMaxRecords)
	if err != nil {
		logging.Warn.Printf("[%s] squad history read failed for %q, serving empty: %v", s.serverConfig.Label, teamID, err)
		return buildHistoryResponse(hi, nil)
	}
	return buildHistoryResponse(hi, evs)
}

func NewSquadHistoryServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadHistoryServer {
	s := &squadHistoryServer{repo: repo}

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
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			buf := s.buildHistory(hi, *readBuffer)
			return &buf, nil
		},
	}

	return s
}
