package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"context"
	"encoding/binary"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Squad registration / creation response.
//
// Reverse-engineered from Release.xex: builder sub_823BE928 (internal message code 181) sends
// "<gamertag>,<userid>,<squadname>,<faction 'A'/'B'/'C'>,<n>,<n>" to port 1212+... actually port
// 1020+181 = 1201. recordSize is 44, so the client allocates a 44-byte record buffer -- the old
// static 532-byte buffer overran it (assert "Buffer over run", CH_DataServerCommunicator.cpp:1364).
//
// Response parser sub_823BCCB0 reads, from the 44-byte body:
//   body[0]      = status: '1' success, '2' "Unique Error" (name taken), other "Unknown Error"
//   body[2..19]  = Team ID (18 bytes, "TM" + 16 digits) -> logged "Team ID:%s"
//   body[21..38] = User ID (18 bytes, "US" + 16 digits) -> logged "User ID:%s"
// bytes [1] and [20] are separators the parser skips. Team-id format is "TM%04d%012I64d"
// (season + 12-digit id); all-zero = no team. See SERVER_ANALYSIS.md.

// SquadRegRecord is the 44-byte squad-registration response body.
type SquadRegRecord struct {
	Status byte     // off 0  - '1' success / '2' unique-error / other unknown-error
	Sep1   byte     // off 1  - separator (skipped by parser)
	TeamID [18]byte // off 2  - "TM" + 16 digits (assigned squad id)
	Sep2   byte     // off 20 - separator (skipped by parser)
	UserID [18]byte // off 21 - "US" + 16 digits (assigned user id)
	_      [5]byte  // off 39 - pad to 44
}

// SquadRegState is the full reply: header(32) + record(44). Total = constants.SquadRegResponseSize (76).
type SquadRegState struct {
	Header MessageHeader
	Record SquadRegRecord
}

func CreateSquadRegState(xuid [16]byte, order [8]byte) SquadRegState {
	rec := SquadRegRecord{Status: '1', Sep1: ',', Sep2: ','}
	// Assign a well-formed team id (season 0001, team #1) and user id. These are placeholders until
	// persistent registration exists; the format must match "TM%04d%012I64d" / "US..." or the
	// login-time team-data validator (sub_823BB678) clears it.
	copy(rec.TeamID[:], "TM0001000000000001")
	copy(rec.UserID[:], "US0001000000000001")

	return SquadRegState{
		Header: CreateHeader(xuid, order),
		Record: rec,
	}
}

type squadRegServer struct {
	*messageServer
}

func NewSquadRegServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *squadRegServer {
	s := &squadRegServer{}

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
			return validateWorldPacket(packet, clientAddr, serverConfig.Label) // CH magic + min-size
		},
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			resp := CreateSquadRegState(hi.Xuid, hi.Order)
			buf := make([]byte, constants.SquadRegResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
