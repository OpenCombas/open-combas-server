package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"context"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Squad status-ack responses: squad-data LOCK (port 1211 = 1020+msgCode 191) and squad CONFIG /
// settings upload (port 1205 = 1020+msgCode 185). Both are tiny acks whose parser inspects only the
// first body byte; the static server always answers success ('1').
//
// Reverse-engineered from Release.xex:
//   - LOCK builder sub_823BEE50 (code 191) sends "<gamertag>,<teamid>,<op>" (e.g. "ibac,TM...,1");
//     recordSize 2; parser sub_823BD8D8 reads body[0]:
//       '1' "Lock Get or Relese Complete" (success), '2' "Other User Locking",
//       '3' "Lock Target None", '5' "Title Server Resetting", else "Unknown Error".
//     This lock op runs whenever the player has a squad and enters the Live menu, so an unanswered
//     1211 makes the client give up and become unable to log in.
//   - CONFIG builder sub_823BEFA0 (code 185) sends a 60-byte binary settings blob (stance, activity,
//     language, regions, role flags, team colours); recordSize -1; parser sub_823BD9D8 reads body[0]:
//       '1' "Team Config Complete" (success), '2' "User Number Error Or Other User Editing",
//       '3' "Target Team Not Exist", else "Unknown Error".
//     This is the "upload settings" step after squad creation.
//
// Neither parser deserialises a body or checks its length beyond body[0], so a 2-byte body is enough.
// Persisting the uploaded 1205 settings blob is deferred to the future stateful/DB layer; for now the
// server just acknowledges them. See project_combas_server_protocol memory.

// SquadAckState is header(32) + a 2-byte status body. Total = constants.SquadAckResponseSize (34).
type SquadAckState struct {
	Header MessageHeader
	Status byte // '1' = success ("Lock ... Complete" / "Team Config Complete")
	_      byte // pad to the 2-byte response record
}

func CreateSquadAckState(xuid [16]byte, order [8]byte) SquadAckState {
	return SquadAckState{
		Header: CreateHeader(xuid, order),
		Status: '1',
	}
}

type squadAckServer struct {
	*messageServer
}

func NewSquadAckServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *squadAckServer {
	s := &squadAckServer{}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,
		responseSize:  constants.SquadAckResponseSize,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateWorldPacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			return CreateSquadAckState(hi.Xuid, hi.Order)
		},
	}

	return s
}
