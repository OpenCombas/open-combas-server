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

// SHOP response (msgCode 188). SCAFFOLD -- the wire STRUCTURE is reverse-engineered and pinned here; the
// field SEMANTICS are not yet known, so this serves a PROVENANCE-MARKER response (distinct, recognisable
// values in every field) rather than a real catalogue. Open the shop in-game against this server and read
// which markers land where in the UI to label each field, then replace buildMarkerShop with the real model.
//
// WIRE (reverse-engineered from Release.xex request sub_823BF4F0 + response parser sub_823BDDE8):
//   - Request  (client -> server): msgCode 188, port = shop base 1060 + 188 = 1248. Body is the text command
//     "%s,%c" == "<account>,<nation>" after the 32-byte header (e.g. "ibac,C"). We don't parse it yet.
//   - Response (server -> client): 2 blocks x 1012 bytes, deserialised LITTLE-ENDIAN by sub_823BDDE8 as a
//     header "C20, I7, S1, C2" (52 B) followed by 80 entries of "I1, S2, C4" (12 B each). Total body 2024 B;
//     with the 32-byte header the reply is 2056 B.
//
// FRAGMENTATION CAVEAT: 2056 B exceeds one ~1024 B combas frame. This sends a single UDP datagram (IP-layer
// fragmentation), which the client's IP stack reassembles into one recvfrom. If the combas layer instead
// requires its own multi-packet framing (the request path caps a request body at <1024 and the parser was
// registered with respSize -1 = variable), the first capture will show it and we add combas-level framing.

const (
	shopBlockItems   = 80                                            // entries per block ("I1, S2, C4" x 80)
	shopItemSize     = 12                                            // I1(4) + S2(4) + C4(4)
	shopBlockHeader  = 52                                            // C20(20) + I7(28) + S1(2) + C2(2)
	shopBlockSize    = shopBlockHeader + shopBlockItems*shopItemSize // 1012
	shopBlockCount   = 2                                             // parser reads a2 and a2+1012
	shopBodySize     = shopBlockCount * shopBlockSize                // 2024
	shopResponseSize = constants.MinHelloMessageSize + shopBodySize  // 2056
)

// ShopItem is one catalogue entry (12 bytes, schema "I1, S2, C4"). Field meanings are provisional -- see the
// marker builder; the in-game capture confirms which is price / id / stock / part-code.
type ShopItem struct {
	Price int32    // "I1" -- provisional: item cost
	IDs   [2]int16 // "S2" -- provisional: item id + a second attribute (stock / level / category)
	Code  [4]byte  // "C4" -- provisional: 4-char part/item code
}

// ShopBlock is one of the two 1012-byte sections (schema "C20, I7, S1, C2" header + 80 items).
type ShopBlock struct {
	Name  [20]byte // "C20" -- provisional: block/category/shop name
	Vals  [7]int32 // "I7"  -- provisional: block-scope figures (funds / counts / flags)
	Count int16    // "S1"  -- provisional: number of valid entries in Items
	Flag  [2]byte  // "C2"  -- provisional: 2-char code/flag
	Items [shopBlockItems]ShopItem
}

// ShopResponse is the full reply: the standard 32-byte header + two blocks.
type ShopResponse struct {
	Header MessageHeader
	Blocks [shopBlockCount]ShopBlock
}

// validPartCodes are 4-char part codes CONFIRMED to exist in the client's part database (reverse-engineered
// from the "Hound Buy Check" sub_822AA968 / CH_ShopLimitedListScene; the client loads
// auto:\game\hound\shop\<code>.mcd). The Code field is validated against the part DB -- an unknown code makes
// the client DROP the whole entry ("no shop lineup"), which is why the all-marker first attempt rendered
// empty. Entries using these codes render; the OTHER fields still carry markers so the capture labels them.
// TODO: expand to the full catalogue from the .mcd files the game loads (see chromehounds_shop_re.md).
var validPartCodes = []string{"H600", "H601", "H602"}

// buildMarkerShop serves a mostly-marker catalogue whose FIRST entries use real part codes so they actually
// render, while their other fields stay distinct markers to label the structure from one capture:
//   - block Name -> "BLOCK0 ..."/"BLOCK1 ..." (which section is which, and whether both render);
//   - block Vals[i] -> 1_000_000*(block+1)+i (find funds/count among the 7);
//   - item Price -> 100_000*(block+1)+i (find the price cell and item order);
//   - item Code -> a real part code for the first len(validPartCodes) entries, else "A000".. (which the
//     client drops) -- so the rendered rows prove the Code field IS the part-lookup key;
//   - item IDs -> {i, 1000+i} (tell the two shorts apart).
//
// Count is set to the full item count; if the UI shows only the valid-coded rows, Count does not gate.
func buildMarkerShop(xuid [16]byte, order [8]byte) ShopResponse {
	resp := ShopResponse{Header: CreateHeader(xuid, order)}
	for b := 0; b < shopBlockCount; b++ {
		blk := &resp.Blocks[b]
		copy(blk.Name[:], fmt.Sprintf("BLOCK%d-NAME-C20xxxx", b)) // <=20 chars
		for i := 0; i < len(blk.Vals); i++ {
			blk.Vals[i] = int32(1_000_000*(b+1) + i)
		}
		blk.Count = int16(shopBlockItems)
		blk.Flag[0] = byte('0' + b)
		blk.Flag[1] = 'F'
		codeCh := byte('A' + b)
		for i := 0; i < shopBlockItems; i++ {
			it := &blk.Items[i]
			it.Price = int32(100_000*(b+1) + i)
			it.IDs[0] = int16(i)
			it.IDs[1] = int16(1000 + i)
			if i < len(validPartCodes) {
				copy(it.Code[:], validPartCodes[i]) // real part -> this row renders
			} else {
				copy(it.Code[:], fmt.Sprintf("%c%03d", codeCh, i)) // unknown -> dropped by the client
			}
		}
	}
	return resp
}

type shopServer struct {
	*messageServer
}

func NewShopServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *shopServer {
	s := &shopServer{}

	// Announce the scaffold state unconditionally -- a capture must never be ambiguous about what it saw.
	logging.Info.Printf("[%s] SHOP scaffold: serving PROVENANCE-MARKER catalogue (no real model yet) -- %d blocks x %d B, reply %d B; request body \"<account>,<nation>\" not yet parsed",
		serverConfig.Label, shopBlockCount, shopBlockSize, shopResponseSize)

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
			return buildMarkerShop(hi.Xuid, hi.Order)
		},
		responseSize: shopResponseSize,
	}

	return s
}

func validateShopPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
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
	if packet[0] != ChromeHoundsHeader[0] || packet[1] != ChromeHoundsHeader[1] {
		err := ValidationError{Reason: "invalid Chromehounds header", Size: packetSize}
		logging.LogPacketValidationError(label, clientAddr, err.Reason, packetSize)
		return err
	}
	return nil
}
