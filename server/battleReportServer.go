package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"context"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Battle-report (mission-result) submission.
//
// Reverse-engineered from Release.xex: builder sub_823BEF10 (internal message code 194) sends a fixed
// 565-byte binary report to port 1020+194 = 1214 (newer revision 1254); recordSize -1; parser
// sub_823BD940. This is the post-mission submission that drives war progression -- the client fires it
// at the end of a conquest sortie and retries 6x; with nothing listening the OS answers ICMP
// port-unreachable and the mission cannot finalise.
//
// REQUEST body (565 bytes, after the 32-byte header), as observed in pcaps/end_mission.pcapng frame 33:
//   +0   gamertag of the submitter, null-padded ("ibac")
//   +16  season id ("0001")
//   +32  battle metadata (counts) followed by a list of participating-squad entries; each entry carries
//        a faction char + team id ("TM"+16) + squad name + member rows (user id "US"+16 + gamertag).
//        Observed: our squad 'B'/"TM0001000000000001"/"OpenCombas" (member "US0001000000000001"/"ibac")
//        and an AI placeholder "AAA"/"9999..."/"DummySquadName"; the tail holds the outcome bytes.
//   Decoding the individual occupation/kill/win-loss fields is deferred to the stateful war-state layer;
//   the static server only needs to acknowledge the report.
//
// RESPONSE body is an ASCII CSV status "<end>,<bf>,<area>,<nation>" read by sub_823BD940 at even offsets:
//   body[0] '1' = "Normal End" (anything else => "Unknown Error")
//   body[2] '1' = "Battle Field" event   (battlefield captured)
//   body[4] '1' = "Area" event           (area captured)
//   body[6] '1' = nation defeated        ("国家勢力を落とした")
// odd offsets are ',' separators. The static server returns "1,0,0,0": accept the report, signal Normal
// End, raise no special conquest events (those will be emitted by the dynamic war engine later).
// See project_combas_server_protocol memory.

// BattleReportAck is header(32) + a 7-byte CSV status body. Total = constants.BattleReportResponseSize (39).
type BattleReportAck struct {
	Header      MessageHeader
	Status      byte // off 0 - '1' = Normal End
	Sep1        byte // off 1 - ',' separator (ignored by parser)
	BattleField byte // off 2 - '1' => battlefield-captured event
	Sep2        byte // off 3 - ','
	Area        byte // off 4 - '1' => area-captured event
	Sep3        byte // off 5 - ','
	Nation      byte // off 6 - '1' => nation-defeated event
}

func CreateBattleReportAck(xuid [16]byte, order [8]byte) BattleReportAck {
	return BattleReportAck{
		Header:      CreateHeader(xuid, order),
		Status:      '1', // Normal End
		Sep1:        ',',
		BattleField: '0',
		Sep2:        ',',
		Area:        '0',
		Sep3:        ',',
		Nation:      '0',
	}
}

type battleReportServer struct {
	*messageServer
}

func NewBattleReportServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *battleReportServer {
	s := &battleReportServer{}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,
		responseSize:  constants.BattleReportResponseSize,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateWorldPacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			return CreateBattleReportAck(hi.Xuid, hi.Order)
		},
	}

	return s
}
