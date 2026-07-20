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

	"github.com/prometheus/client_golang/prometheus"
)

// Squad ranking / leaderboard (msgCode 202, port 1020+202 = 1222 debug; retail 1262).
//
// Reverse-engineered from Release.xex: CH_OnlineWorldSituationInfoSquadRanking issues the query
// (sub_823C77E8 -> builder sub_823BF748), host-only. Response parser sub_823BDF18 reads the 2240-byte
// body as two 1120-byte blocks, each with an "I2,C4" header at +0 and an "I50" (50 int32) value array at
// +916. See SQUAD_STATS_DESIGN.md.
//
// REQUEST body: "<gamertag>,<teamID>,<SEA>,<KBN>"  (e.g. "ibac2,TM0001000000000005,1,1")
//
//	SEA: 1 = Total, 2 = current Season      KBN: 1 = Renown, 2 = Capture Points, 3 = Renown-Per-Member
//
// RESPONSE: header(32) + block0 + block1 (each 1120 bytes).
//
// [LAYOUT CORRECTED 2026-07-20.] The previous layout here (status@+0, count@+4, season@+8, 18-byte names
// @+16) was wrong in every field and rendered garbage on the ranking screen. The corrected layout was read
// at instruction level from the render loop sub_821EDB38 (block base = obj+2332, stride 1120) and is
// verified by closing exactly on 1120 bytes with no gap: 13+50=63, 63+800=863, 916+200=1116.
// Full derivation: workspaces/chromehounds_ranking_re.md.
//
//	+0    int32   THE REQUESTER'S OWN global rank   -- header "I2,C4", rendered only when rank > 100
//	+4    int32   the requester's own value
//	+8    u8      the requester's own grade
//	+12   u8      record count in THIS block                (sub_821EDB38 sums *(base+12) over both blocks)
//	+13+i u8      nation 'A'|'B'|'C'                        (rendered as the nation icon)
//	+63   50 x 16 squad name, NUL-terminated                (v22 = 16*i + base + 63)
//	+863+i u8     grade, rendered as FMG 5700+grade         (same FMG base as the squad panel)
//	+913  3 bytes pad
//	+916  50 x int32 stat value, little-endian              (addressed as 4*(i+229)+base)
//	+1116 u8      status, 0 = success                       (non-zero on EITHER block = error path)
//
// The two blocks are ranks 1-50 and 51-100 of ONE descending list -- not "top-50 + your entry". Proven by
// the render loop's tie check reading block1_base-8 (= block0's value[49]) for the first entry of block 1,
// which is only meaningful if the blocks are contiguous slices.
//
// There is NO paging: the request carries no offset/limit and the client is hard-capped at 100 entries.
const (
	rankingBlockSize    = 1120
	rankingBlocks       = 2
	rankingBodySize     = rankingBlockSize * rankingBlocks                // 2240
	rankingResponseSize = constants.MinHelloMessageSize + rankingBodySize // 2272
	rankingMaxEntries   = 50                                              // per block; 100 total
	rankingTotalEntries = rankingMaxEntries * rankingBlocks

	rankingOwnRankOffset  = 0    // int32, requester's own global rank
	rankingOwnValueOffset = 4    // int32, requester's own value
	rankingOwnGradeOffset = 8    // u8,    requester's own grade
	rankingCountOffset    = 12   // u8,    records in this block
	rankingNationOffset   = 13   // u8 x 50
	rankingNameOffset     = 63   // 16 bytes x 50
	rankingNameStride     = 16   //
	rankingGradeOffset    = 863  // u8 x 50
	rankingValueOffset    = 916  // int32 x 50
	rankingStatusOffset   = 1116 // u8, 0 = success
)

// parseSquadRanking pulls (teamID, SEA-is-season, KBN stat) from the request body.
func parseSquadRanking(packet []byte) (teamID string, kbn int, useSeason, ok bool) {
	if len(packet) <= constants.MinHelloMessageSize {
		return "", 0, false, false
	}
	body := packet[constants.MinHelloMessageSize:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(string(body), ",")
	if len(parts) < 4 {
		return "", 0, false, false
	}
	teamID = strings.TrimSpace(parts[1])
	sea, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
	kbn, _ = strconv.Atoi(strings.TrimSpace(parts[3]))
	return teamID, kbn, sea == 2, teamID != "" && kbn >= 1 && kbn <= 3
}

// writeRankBlock encodes one 1120-byte block: the per-entry arrays plus the count and the success status.
// The block-0-only "your own standing" header is written separately by writeOwnStanding, since block 1
// must not carry it.
//
// Names are truncated to 15 bytes so the 16-byte slot always keeps a NUL terminator -- the render reads
// them as C strings and an unterminated slot would run into the next name.
func writeRankBlock(block []byte, entries []RankEntry) {
	n := len(entries)
	if n > rankingMaxEntries {
		n = rankingMaxEntries
	}
	block[rankingStatusOffset] = 0 // 0 = success; non-zero on either block sends the client down the error path
	block[rankingCountOffset] = byte(n)
	for i := 0; i < n; i++ {
		e := entries[i]
		block[rankingNationOffset+i] = e.Nation
		block[rankingGradeOffset+i] = e.Grade

		nameOff := rankingNameOffset + i*rankingNameStride
		name := e.Name
		if len(name) > rankingNameStride-1 {
			name = name[:rankingNameStride-1]
		}
		copy(block[nameOff:nameOff+rankingNameStride], name)

		binary.LittleEndian.PutUint32(block[rankingValueOffset+i*4:], uint32(e.Value))
	}
}

// paginateRanking splits one descending list into the two blocks: ranks 1-50 and ranks 51-100.
//
// Block 1 is only non-empty once block 0 is FULL. The client's tie check reads across the seam (it takes
// the value at block1_base-8, i.e. block0's entry 49, as the predecessor of block 1's first entry), so the
// blocks must be contiguous slices of one list. Putting anything else in block 1 -- as the previous
// "top-50 + your own entry" model did -- corrupts the rank numbering the client derives.
//
// Anything past 100 is dropped: the request carries no offset/limit and the client is hard-capped at 100.
func paginateRanking(entries []RankEntry) (first, second []RankEntry) {
	if len(entries) > rankingTotalEntries {
		entries = entries[:rankingTotalEntries]
	}
	if len(entries) > rankingMaxEntries {
		return entries[:rankingMaxEntries], entries[rankingMaxEntries:]
	}
	return entries, nil
}

// writeOwnStanding fills block 0's header with the requesting squad's own global standing. The client only
// renders it when the rank exceeds 100 (i.e. the squad is off the end of the returned list), so it must
// carry the TRUE global rank rather than one clamped to what was returned. rank is 1-based; 0 means the
// requester is not ranked and the header is left zeroed.
func writeOwnStanding(block0 []byte, rank int, e RankEntry) {
	if rank <= 0 {
		return
	}
	binary.LittleEndian.PutUint32(block0[rankingOwnRankOffset:], uint32(rank))
	binary.LittleEndian.PutUint32(block0[rankingOwnValueOffset:], uint32(e.Value))
	block0[rankingOwnGradeOffset] = e.Grade
}

type squadRankingServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> empty leaderboard
}

func (s *squadRankingServer) buildResponse(hi UserHelloMessage, packet []byte) []byte {
	resp := make([]byte, rankingResponseSize)
	if _, err := binary.Encode(resp, binary.LittleEndian, CreateHeader(hi.Xuid, hi.Order)); err != nil {
		return resp
	}
	body := resp[constants.MinHelloMessageSize:]
	block0, block1 := body[0:rankingBlockSize], body[rankingBlockSize:rankingBodySize]

	teamID, kbn, useSeason, ok := parseSquadRanking(packet)
	if !ok || s.repo == nil {
		return resp // empty (count 0) leaderboard is a valid reply
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	entries, err := s.repo.RankSquads(readCtx, kbn, useSeason, currentSeason)
	if err != nil {
		logging.Warn.Printf("[%s] ranking query failed, empty leaderboard: %v", s.serverConfig.Label, err)
		return resp
	}

	yourRank := 0
	for i, e := range entries {
		if e.TeamID == teamID {
			yourRank = i + 1
			break
		}
	}

	first, second := paginateRanking(entries)
	writeRankBlock(block0, first)
	writeRankBlock(block1, second)
	if yourRank > 0 {
		writeOwnStanding(block0, yourRank, entries[yourRank-1])
	}
	logging.Info.Printf("[%s] ranking kbn=%d season=%v -> %d squads (%q rank %d)", s.serverConfig.Label, kbn, useSeason, len(entries), teamID, yourRank)
	return resp
}

func NewSquadRankingServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadRankingServer {
	s := &squadRankingServer{repo: repo}

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
			buf := s.buildResponse(hi, *readBuffer)
			return &buf, nil
		},
	}

	return s
}
