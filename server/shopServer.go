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

// ShopItem is one catalogue entry (12 bytes, schema "I1, S2, C4"). The "C4" field is NOT a 4-char string --
// it is the 4-byte PACKED PART DESCRIPTOR the client resolves against its runtime part DB (unk_829EB580):
// byte0 = major category (1-6), byte1 = subcategory, bytes2-3 = index-within-subcategory (little-endian on
// the wire, so as a uint32 value it reads back as 0xMMSSIIII, e.g. 0x01010001). sub_82294F28 derives the
// display category from byte0/byte1; the lineup check matches the whole descriptor against a real part.
type ShopItem struct {
	Price int32    // "I1" -- item cost (buy scene compares against funds)
	IDs   [2]int16 // "S2" -- two shorts (id / attribute); not read by the featured-part path
	Desc  uint32   // "C4" -- packed part descriptor 0xMMSSIIII (major<<24 | sub<<16 | index)
}

// ShopBlock is one of the two 1012-byte sections. On the wire it deserializes as "C20, I7, S1, C2" + 80x
// "I1,S2,C4", but the shop's own parser (sub_82162118) reads it field-by-field, which pins these meanings:
//   - +0  Status  MUST be 0. If block0[0] || block1[0] is non-zero the client rejects the ENTIRE reply with
//     the "failure to parse" dialog (text 1320) -- this is the C20 field's first byte (a status byte, like
//     every other combas response), NOT part of a name. Getting this wrong was the whole bug.
//   - +50 Count is a BYTE (the C2 field's first byte) = how many of the 80 Items the client actually reads.
//     (The int16 at +48 -- schema S1 -- is not read by the lineup parser.)
type ShopBlock struct {
	Status byte     // +0  status; 0 = OK (non-zero => whole reply rejected)
	Name   [19]byte // +1  19-byte string field (season/shop id; sub_82828640 copies it)
	Vals   [7]int32 // +20 block values (I7): +20 funds?, +24..40 five "new parts" slots, +44 ...
	S1     int16    // +48 (schema S1; not read by the lineup parser)
	Count  byte     // +50 ENTRY COUNT: number of Items the client reads (0..80)
	Flag   byte     // +51 (C2[1]; TBD)
	Items  [shopBlockItems]ShopItem
}

// ShopResponse is the full reply: the standard 32-byte header + two blocks.
type ShopResponse struct {
	Header MessageHeader
	Blocks [shopBlockCount]ShopBlock
}

// shopCatalogue is a curated spread of REAL packed part descriptors, dumped live from the client's runtime
// part DB (unk_829EB580, 603 parts) via its own index accessor sub_82284200. It takes the first descriptors
// of every non-empty (major.sub) group, so the set spans all 6 major categories and all 41 subcategories --
// every hound part slot type is represented. Each value is 0xMMSSIIII (major, sub, index). Expand toward the
// full 603 as needed; see project_shop_subsystem memory + scratchpad/mcd_parts.json for the complete dump.
var shopCatalogue = []uint32{
	0x01010001, 0x01010002, 0x01020001, 0x01020002, 0x0103001F, 0x01030020, 0x02010001, 0x02010002,
	0x02020001, 0x02020002, 0x02030001, 0x02030002, 0x02040001, 0x02040002, 0x02050001, 0x02050002,
	0x02060001, 0x02060002, 0x03010001, 0x03010002, 0x03020001, 0x03020002, 0x03030001, 0x03030002,
	0x04010001, 0x04010002, 0x04020001, 0x04020002, 0x04030001, 0x04030002, 0x05010001, 0x05010002,
	0x05020001, 0x05020002, 0x05030001, 0x05030002, 0x05040001, 0x05040002, 0x05050001, 0x05050002,
	0x05060001, 0x05060002, 0x05070001, 0x05070002, 0x05080001, 0x05080002, 0x05090001, 0x05090002,
	0x050A0001, 0x050A0002, 0x050B0001, 0x050B0002, 0x050C0001, 0x050C0002, 0x050D0001, 0x050D001F,
	0x050E0001, 0x050E0002, 0x050F0002, 0x050F0021, 0x05100001, 0x05100002, 0x05110001, 0x05110002,
	0x06010001, 0x06010002, 0x06020001, 0x06020002, 0x06030001, 0x06030002, 0x06040001, 0x06040002,
	0x06050001, 0x06050002, 0x06060001, 0x06060002, 0x06080001, 0x06080002, 0x06090001, 0x06090002,
	0x060A0001, 0x060A0002,
}

// buildShop serves a real catalogue built from shopCatalogue. The client consumes the reply two ways
// (sub_82162118 / sub_82163180): the 5 "New Parts" dwords at block+24 (Vals[1..5]) are the FEATURED pool --
// the shop RNG-picks one to show as the visit's new part -- and the Items list (Count entries) is the
// browsable lineup, each keyed by its packed Desc. The 82-descriptor spread is split across the two blocks;
// each block's featured pool is drawn from its own slice so the featured part is always a real, loadable part.
func buildShop(xuid [16]byte, order [8]byte) ShopResponse {
	resp := ShopResponse{Header: CreateHeader(xuid, order)}
	half := (len(shopCatalogue) + 1) / 2
	for b := 0; b < shopBlockCount; b++ {
		blk := &resp.Blocks[b]
		blk.Status = 0 // REQUIRED: non-zero here makes the client reject the whole reply

		lo := b * half
		hi := lo + half
		if hi > len(shopCatalogue) {
			hi = len(shopCatalogue)
		}
		slice := shopCatalogue[lo:hi]

		copy(blk.Name[:], fmt.Sprintf("COMBAS SHOP %d", b))
		// Vals[0] (+20) and Vals[6] (+44) are shop params of unconfirmed meaning; leave 0. Vals[1..5] (+24)
		// are the 5 featured "New Parts" -- fill from this block's slice so each is a real descriptor.
		for i := 0; i < 5 && i < len(slice); i++ {
			blk.Vals[1+i] = int32(slice[i])
		}
		blk.Count = byte(len(slice)) // BYTE at +50 -- the client reads this many Items
		for i, desc := range slice {
			it := &blk.Items[i]
			it.Price = 1000 // nominal cost; featured-part path ignores it
			it.Desc = desc
		}
	}
	return resp
}

type shopServer struct {
	*messageServer
}

func NewShopServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) *shopServer {
	s := &shopServer{}

	// Announce state unconditionally -- a capture must never be ambiguous about what it saw.
	logging.Info.Printf("[%s] SHOP: serving %d real part descriptors across %d blocks x %d B (reply %d B); featured pool = Vals[1..5], lineup = Items",
		serverConfig.Label, len(shopCatalogue), shopBlockCount, shopBlockSize, shopResponseSize)

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
			return buildShop(hi.Xuid, hi.Order)
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
