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
	CountryCode       byte    // off 0  - 'A'/'B'/'C' = Tarakia/Morskoj/Sal Kar (flag/name/description)
	_                 [3]byte // align
	TotalIncome       int32   // off 4  - "Total Revenue"
	FixedIncome       int32   // off 8  - (not shown on faction screen; economic input)
	NumberOfAreas     byte    // off 12 - drives "Control %" (= areas / total areas, ~22 total)
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
		// presidentID is a placeholder index per nation (Tarakia's #1 = "Glen Conrad");
		// the correct per-nation IDs are tunable.
		mkNation('A', 1, 1_000_000, 5_000, 120), // Tarakia
		mkNation('B', 2, 900_000, 4_500, 100),   // Morskoj
		mkNation('C', 3, 1_100_000, 5_500, 140), // Sal Kar
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
