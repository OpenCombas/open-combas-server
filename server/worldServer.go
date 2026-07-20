package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// World Situation response (ports WORLD 1215 / WORLD_OLD 1255).
//
// Reverse-engineered from Release.xex: the client parses this reply in sub_823BD228
// (internal message code 195) via the byte-swap deserializer sub_823B55E8. All multi-byte
// fields are LITTLE-ENDIAN on the wire and laid out with C-struct natural alignment
// (explicit pad fields below). See open-combas-server/SERVER_ANALYSIS.md §4c and §6.
//
// NOTE: the ranking / election / donation sub-sections that occupy the tail of the body
// (client offsets 292 / 440 / 480 / 516) are not yet field-labelled; they are zero-filled
// here via WorldState.Tail so the overall body size matches what the client allocates (540).

// NationData is one faction record (60 bytes). Wire format string "C,I2,C,I5,S,I4,C3".
// Labels confirmed in-game from the "Select Allegiance" / faction-detail screens.
type NationData struct {
	CountryCode   byte    // off 0  - 'A'/'B'/'C' = Tarakia/Morskoj/Sal Kar (flag/name/description)
	_             [3]byte // align
	TotalIncome   int32   // off 4  - "Total Revenue"
	FixedIncome   int32   // off 8  - (not shown on faction screen; economic input)
	NumberOfAreas byte    // off 12 - client renders "Control %" as this / total areas (~22). We now
	//                          populate it with the nation's share of the map's total occupation points
	//                          (StrategicValue-weighted, scaled onto /22) -- see controlAreasFromBattlefields.
	_                 [3]byte // align
	Field16           float32 // off 16 - float, purpose TBD (not on faction screen; was 0.00)
	ExchangeRate      int32   // off 20 - "Exchange Rate" (¥)
	Population        int32   // off 24 - "Population"
	NumberOfSoldiers  int32   // off 28 - "Troop Strength"
	NumberOfPlayers   int32   // off 32 - (not shown on faction screen)
	ResearchLevel     uint16  // off 36 - "Research Level"
	_                 [2]byte // align
	ResearchBudget    int32   // off 40 - "Research Budget"
	MaintenanceBudget int32   // off 44 - "Maintenance Budget"
	MilitaryBudget    int32   // off 48 - "Military Budget"
	PriceIndex        float32 // off 52 - "Price Index" (shown as %.2f)
	PresidentID       byte    // off 56 - president index (global president table)
	Unknown57         byte    // off 57 - purpose TBD
	DeadFlag          byte    // off 58 - nation eliminated flag
	_                 byte    // pad to 60
}

// WorldHeader is the body header (28 bytes). Wire format string "C21,I".
type WorldHeader struct {
	Status          byte     // 0 = OK / parsed
	SeasonID        [19]byte // ASCII, NUL-terminated
	ServerResetFlag byte     // off 20
	_               [3]byte  // align
	DataTime        int32    // off 24
}

// WorldState is the full reply. Header(32) + body(540). Body = WorldHeader(28) +
// 3 nations(180) + Tail(332). Total encoded size = constants.WorldResponseSize (572).
type WorldState struct {
	Header  MessageHeader
	World   WorldHeader
	Nations [3]NationData
	Tail    [332]byte // ranking / election / donation sections (TBD)
}

// worldHeaderNow is the common 28-byte body header (season + reset flag + timestamp) shared by the
// world / area / area-info responses.
func worldHeaderNow() WorldHeader {
	var seasonID [19]byte
	copy(seasonID[:], "0001") // tunable; the client prints this as "Season ID: %s"
	return WorldHeader{
		Status:          0,
		SeasonID:        seasonID,
		ServerResetFlag: 0,
		DataTime:        int32(time.Now().Unix()),
	}
}

// PRESIDENT TABLE. PresidentID is a KEY into the table the client loads from
// menu/WorldSituationInfoNewsPresidentParam.bin -- not an index, and not per-nation-relative. The client
// looks it up by linear scan (sub_822A1D20) and reports "PRESIDENT ID (%u) : INVALID PARAMETER" on a miss.
//
// That file defines 35 presidents in three contiguous per-nation blocks, each record mapping the key to an
// FMG string id:
//
//	ids  1..13 -> FMG 8200..8212  Tarakia            (Glen Conrad, ...)
//	ids 14..25 -> FMG 8400..8411  Rep. of Morskoj    (Viktor Barsukov, ...)
//	ids 26..35 -> FMG 8600..8609  Sal Kar            (Asad Aslan, ...)
//
// The blocks are 13/12/10 -- deliberately unequal, so there is no stride to compute one from. Serving
// 1/2/3 for the three nations (the old placeholder) put every nation inside TARAKIA's block, which is why
// all three displayed Tarakian presidents.
const (
	presidentTarakiaFirst = 1
	presidentTarakiaLast  = 13
	presidentMorskojFirst = 14
	presidentMorskojLast  = 25
	presidentSalKarFirst  = 26
	presidentSalKarLast   = 35
)

// presidentRangeFor returns the valid president-key range for a nation's country code.
func presidentRangeFor(code byte) (first, last byte, ok bool) {
	switch code {
	case 'A':
		return presidentTarakiaFirst, presidentTarakiaLast, true
	case 'B':
		return presidentMorskojFirst, presidentMorskojLast, true
	case 'C':
		return presidentSalKarFirst, presidentSalKarLast, true
	}
	return 0, 0, false
}

// clampPresidentID keeps a stored president key inside its own nation's block. Persisted nation docs seeded
// before this mapping was known hold 1/2/3, which renders as three Tarakian presidents rather than failing
// visibly -- so an out-of-block value is corrected to the nation's first president instead of being served.
// An unknown country code is passed through unchanged; there is nothing better to map it to.
func clampPresidentID(code byte, id byte) byte {
	first, last, ok := presidentRangeFor(code)
	if !ok {
		return id
	}
	if id < first || id > last {
		return first
	}
	return id
}

// defaultNations is the static faction model. It is both the fallback when Mongo is unavailable and the
// seed source for the `nations` collection (see seedNations).
func defaultNations() [3]NationData {
	// Plausible placeholder values so the in-game World Situation screen shows data.
	mkNation := func(code byte, presidentID byte, pop, soldiers, players int32) NationData {
		return NationData{
			CountryCode:       code,
			TotalIncome:       1_000_000, // Total Revenue
			FixedIncome:       500_000,
			NumberOfAreas:     5, // -> Control %
			ExchangeRate:      100,
			Population:        pop,
			NumberOfSoldiers:  soldiers,
			NumberOfPlayers:   players,
			ResearchLevel:     3,
			ResearchBudget:    200_000,
			MaintenanceBudget: 150_000,
			MilitaryBudget:    300_000,
			PriceIndex:        0,
			PresidentID:       presidentID, // per-nation index into the president table
			DeadFlag:          0,
		}
	}
	return [3]NationData{
		// Each nation's FIRST president, from its own block of the president table (see the block map
		// above). These are the only three values that are correct-by-construction; any other choice must
		// stay inside the nation's range or the client renders another nation's leader.
		mkNation('A', presidentTarakiaFirst, 1_000_000, 5_000, 120), // Tarakia -> "Glen Conrad"
		mkNation('B', presidentMorskojFirst, 900_000, 4_500, 100),   // Morskoj -> "Viktor Barsukov"
		mkNation('C', presidentSalKarFirst, 1_100_000, 5_500, 140),  // Sal Kar -> "Asad Aslan"
	}
}

// newWorldState assembles the reply from a fixed set of nations (static fallback or Mongo-backed).
func newWorldState(xuid [16]byte, order [8]byte, nations [3]NationData) WorldState {
	return WorldState{
		Header:  CreateHeader(xuid, order),
		World:   worldHeaderNow(),
		Nations: nations,
	}
}

func CreateWorldState(xuid [16]byte, order [8]byte) WorldState {
	return newWorldState(xuid, order, defaultNations())
}

type worldServer struct {
	*messageServer
	repo *WorldRepository // nil when Mongo is disabled -> static model
}

// buildWorld serves the World Situation reply from Mongo when a repository is wired, falling back to the
// static model on any read error so a Mongo hiccup never drops the response.
func (s *worldServer) buildWorld(hi UserHelloMessage) WorldState {
	if s.repo != nil {
		readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
		defer cancel()
		if recs, err := s.repo.Nations(readCtx); err != nil {
			logging.Warn.Printf("[%s] mongo read failed, using static model: %v", s.serverConfig.Label, err)
		} else if len(recs) == 3 {
			var nations [3]NationData
			for i, r := range recs {
				nations[i] = r.toNationData()
			}
			// Control % (NumberOfAreas) = each nation's share of the map's total occupation points,
			// derived live from battlefield occupation. Falls back to the stored NumberOfAreas if the
			// derivation can't run, so a battlefield-read hiccup never drops the world reply.
			if ca, cb, cc, err := s.repo.NationControlAreas(readCtx); err != nil {
				logging.Warn.Printf("[%s] control-share derive failed, keeping stored NumberOfAreas: %v", s.serverConfig.Label, err)
			} else {
				for i := range nations {
					switch nations[i].CountryCode {
					case 'A':
						nations[i].NumberOfAreas = ca
					case 'B':
						nations[i].NumberOfAreas = cb
					case 'C':
						nations[i].NumberOfAreas = cc
					}
				}
			}
			return newWorldState(hi.Xuid, hi.Order, nations)
		}
	}
	return CreateWorldState(hi.Xuid, hi.Order)
}

func NewWorldServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *WorldRepository) *worldServer {
	s := &worldServer{repo: repo}

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
			return s.buildWorld(hi)
		},
		responseSize: constants.WorldResponseSize,
	}

	return s
}

func validateWorldPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
	packetSize := len(packet)

	if packetSize < constants.MinHelloMessageSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too small (minimum: %d bytes)", constants.MinHelloMessageSize),
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	if packetSize > constants.MaxBufferSize {
		err := ValidationError{
			Reason: fmt.Sprintf("packet too large (maximum: %d bytes)", constants.MaxBufferSize),
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	expectedHeader := ChromeHoundsHeader
	if packet[0] != expectedHeader[0] || packet[1] != expectedHeader[1] {
		err := ValidationError{
			Reason: "invalid Chromehounds header",
			Size:   packetSize,
		}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}

	return nil
}
