package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
	"ChromehoundsStatusServer/shop"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ShopResponseSize is the fixed size of a shop reply: 0x20 header plus two
// 0x3f4 sections.
const ShopResponseSize = 0x20 + 0x3f4*2

// shopLineup is the per-faction buy list, built once at startup. Each server
// answers with the same list; only the faction header of the request differs.
var shopLineup = map[string][]shop.Record{
	"A": shop.CurrentLineup("A"),
	"B": shop.CurrentLineup("B"),
	"C": shop.CurrentLineup("C"),
}

// RunShopServer serves the shop endpoints the game polls for the buy list.
// Ports (label -> port): SHOP_SERVER_NO_UPDATE 1208, SHOP_SERVER_UPDATE 1257,
// SHOP_SERVER_RETAIL 1388, SHOP_SERVER_RETAIL_TITLE_UPDATE 1397.
func RunShopServer(listenAddress net.IP, serverConfig *config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer) {
	shopResponsesHandled := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "shop_responses_handled_total",
		Help: "Total number of shop responses handled",
	})
	shopRequestsPerFaction := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "shop_requests_total",
		Help: "Total number of shop requests per faction",
	}, []string{"faction"})
	wg.Add(1)
	defer wg.Done()

	enablePerfMonitoring := loggingConfig.EnablePerformanceMonitoring
	verboseLogging := loggingConfig.Verbose
	label := serverConfig.Label

	conn, err := buildUDPListener(listenAddress, serverConfig.Port, label, bufferSize)
	if err != nil {
		return
	}
	defer conn.Close()

	readBuffer := pooling.ReadBufferPool.Get()
	defer pooling.ReadBufferPool.Put(readBuffer)

	var startTime time.Time
	var processingTime time.Duration

	for {
		select {
		case <-ctx.Done():
			if verboseLogging {
				logging.LogShutdown(label)
			}
			return

		default:
			if enablePerfMonitoring {
				startTime = time.Now()
			}

			n, clientAddr, err := readUDP(conn, &readBuffer, label)
			if err != nil {
				if !isTimeoutError(err) && enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			packet := readBuffer[:n]

			if err := ValidateShopPacket(packet, clientAddr, label); err != nil {
				if verboseLogging {
					logging.LogPacketValidationError(label, clientAddr, err.Error(), n)
				}
				if enablePerfMonitoring {
					profiling.RecordError()
				}
				continue
			}

			if enablePerfMonitoring {
				processingTime = time.Since(startTime)
				profiling.RecordPacketProcessed(n, processingTime)
			}
			if verboseLogging {
				logging.LogPacketReceived(label, clientAddr, n, processingTime)
			}

			faction := decodeFaction(packet)
			sendBuffer := shop.BuildResponse(packet, shopLineup[faction])
			sendUDP(conn, clientAddr, &sendBuffer, label, true)
			if promConfig.Enabled {
				shopResponsesHandled.Inc()
				shopRequestsPerFaction.WithLabelValues(faction).Inc()
			}
		}
	}
}

// decodeFaction pulls the country code (A/B/C) out of a shop request header.
// The three faction bytes sit right after the gamertag; when none is present
// the Tarakia list is served, matching the TypeScript server's fallback.
func decodeFaction(packet []byte) string {
	for i := len(packet) - 1; i >= 0; i-- {
		switch packet[i] {
		case 'A', 'B', 'C':
			return string(packet[i])
		}
	}
	return "A"
}

// ValidateShopPacket validates incoming shop requests: header present, size in
// range.
func ValidateShopPacket(packet []byte, clientAddr *net.UDPAddr, label string) error {
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

	return nil
}
