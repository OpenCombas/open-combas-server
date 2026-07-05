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
//   SEA: 1 = Total, 2 = current Season      KBN: 1 = Renown, 2 = Capture Points, 3 = Renown-Per-Member
//
// RESPONSE: header(32) + block0 + block1 (each 1120 bytes). block0 = top-50 ranking, block1 = the
// requesting squad's own entry (so the UI can show "your rank" outside the top 50). Per block, the parser
// sub_823BDF18 byte-swaps "I2,C4" at +0 (I = int32, per the login schema convention) and "I50" at +916:
//   +0x000  int32 count, int32 yourRank              ("I2")
//   +0x008  4-char season code                        ("C4")
//   +0x00C  4 reserved bytes  (header struct is 16B: 16 + 50*18 = 916 = the value array start)
//   +0x010  50 x squad-name slots (raw ASCII, null-padded, 18 bytes each)
//   +0x394  50 x int32 stat value (little-endian, "I50")
//
// The 50 int32 VALUES are the authoritative field (they validate the accumulation). Whether count is the
// first or second I2 int32 is the last ⚠ (kept obvious to swap); everything else is confirmed.
const (
	rankingBlockSize    = 1120
	rankingBlocks       = 2
	rankingBodySize     = rankingBlockSize * rankingBlocks                // 2240
	rankingResponseSize = constants.MinHelloMessageSize + rankingBodySize // 2272
	rankingMaxEntries   = 50
	rankingSeasonOffset = 8   // C4 season code (after the two I2 int32s)
	rankingNameOffset   = 16  // names start here (16-byte header; 16 + 50*18 = 916 = value array)
	rankingNameStride   = 18  // per-name slot width
	rankingValueOffset  = 916 // I50 value array (confirmed by parser sub_823BDF18)
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

// writeRankBlock encodes one 1120-byte block. The "I2" header is [status, count] (same pattern as the
// News list reply, sub_823C6EB8): field0 = status (0 = success), field1 = record count. The render only
// displays a block when its status is 0 and count > 0. C4 = the season code.
func writeRankBlock(block []byte, entries []RankEntry) {
	n := len(entries)
	if n > rankingMaxEntries {
		n = rankingMaxEntries
	}
	binary.LittleEndian.PutUint32(block[0:], 0)                          // I2 field 0: status (0 = success)
	binary.LittleEndian.PutUint32(block[4:], uint32(n))                  // I2 field 1: valid record count
	copy(block[rankingSeasonOffset:rankingSeasonOffset+4], currentSeason) // C4 season code
	for i := 0; i < n; i++ {
		nameOff := rankingNameOffset + i*rankingNameStride
		copy(block[nameOff:nameOff+rankingNameStride], entries[i].Name)
		binary.LittleEndian.PutUint32(block[rankingValueOffset+i*4:], uint32(entries[i].Value))
	}
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

	top := entries
	if len(top) > rankingMaxEntries {
		top = top[:rankingMaxEntries]
	}
	writeRankBlock(block0, top) // block 0 = top list
	if yourRank > 0 {
		writeRankBlock(block1, []RankEntry{entries[yourRank-1]}) // block 1 = your entry
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
