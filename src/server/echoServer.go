package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"context"
	"net"
	"sync"
)

func RunEchoingServer(listenAddress net.IP, config *config.ServerConfig, bufferSize int, ctx *context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	cfg := *config
	conn, err := buildUDPListener(listenAddress, cfg.Port, cfg.Label)
	if err != nil {
		return
	}
	defer conn.Close()

	buffer := make([]byte, bufferSize)
	for {
		select {
		case <-(*ctx).Done():
			logging.Info.Printf("[%s] Received shutdown signal\n", cfg.Label)
			return

		default:
			n, clientAddr, err := readUDP(conn, &buffer, cfg.Label)
			if err != nil {
				continue
			}
			var sendBuffer = buffer[:n]
			sendUDP(conn, clientAddr, &sendBuffer, cfg.Label, false)

		}
	}
}
