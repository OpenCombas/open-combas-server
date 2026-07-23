package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// LOT-INFO response (msgCode 190). SCAFFOLD -- the shop requests this SECOND, right after the lineup (188):
// once the lineup parses, sub_82163180 fires "access server data __ get lot info !" and waits (state 8); with
// no reply it hits "[shop] : shop server time out!". So this must be served to unblock the shop.
//
// WIRE (reverse-engineered from request sub_823BF688 + response parser sub_823BDED8):
//   - Request  (client -> server): msgCode 190, port = base + 190 (1210 at base 1020; 1250 at base 1060).
//     Body "%s,%s,%s" = "<account>,<squad>,<...>" (e.g. "ibacinstall,TM0...,..."). Not parsed yet.
//   - Response (server -> client): parser sub_823BDED8 reads, starting at body+4, 40 entries of "I1, C20"
//     (int32 + 20-byte string, 24 B each). Body = 4-byte prefix + 40*24 = 964 B; reply incl. header = 996 B.
//     Field meanings provisional; the first prefix byte is served 0 (a status byte, as on the shop lineup).
const (
	lotInfoItems        = 40
	lotInfoItemSize     = 24                                            // I1(4) + C20(20)
	lotInfoPrefix       = 4                                             // 4-byte prefix before entries (parse starts at body+4)
	lotInfoBodySize     = lotInfoPrefix + lotInfoItems*lotInfoItemSize  // 964
	lotInfoResponseSize = constants.MinHelloMessageSize + lotInfoBodySize // 996
)

// LotInfoItem is one lot entry (24 bytes, schema "I1, C20").
type LotInfoItem struct {
	Value int32    // "I1" -- provisional
	Name  [20]byte // "C20" -- provisional (part/lot code or name)
}

// LotInfoResponse is the full reply: 32-byte header + 4-byte prefix + 40 entries.
type LotInfoResponse struct {
	Header MessageHeader
	Status byte    // +0 status; 0 = OK (mirrors the shop-lineup status byte)
	_      [3]byte // +1..3 pad (entries begin at body+4)
	Items  [lotInfoItems]LotInfoItem
}

func buildMarkerLotInfo(xuid [16]byte, order [8]byte) LotInfoResponse {
	resp := LotInfoResponse{Header: CreateHeader(xuid, order)}
	resp.Status = 0
	for i := 0; i < lotInfoItems; i++ {
		resp.Items[i].Value = int32(200_000 + i)
		copy(resp.Items[i].Name[:], fmt.Sprintf("LOT%02d-NAME-C20xxxx", i))
	}
	return resp
}

type lotInfoServer struct {
	*messageServer
}

func NewLotInfoServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *lotInfoServer {
	s := &lotInfoServer{}

	logging.Info.Printf("[%s] LOT-INFO scaffold: serving marker response (%d entries, reply %d B) to unblock the shop after its lineup request",
		serverConfig.Label, lotInfoItems, lotInfoResponseSize)

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
			return validateShopPacket(packet, clientAddr, serverConfig.Label)
		},
		buildPayload: func(hi UserHelloMessage) interface{} {
			return buildMarkerLotInfo(hi.Xuid, hi.Order)
		},
		responseSize: lotInfoResponseSize,
	}

	return s
}
