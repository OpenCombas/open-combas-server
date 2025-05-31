package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/server"
	"context"
	"net"
	"os/signal"
	"sync"
	"syscall"
)

var wg sync.WaitGroup

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	defer stop()

	logging.Info.Println("App started")
	var cfg = config.LoadConfig()
	logging.Info.Println("Config Loaded")
	var address = net.ParseIP(cfg.ListeningAddress)
	for _, serverConfig := range cfg.Servers {
		if serverConfig.Enabled {
			bufferSize := getBufferSize(serverConfig.BufferSize, cfg.DefaultBufferSize)
			switch serverConfig.Type {
			case config.Status:
				go server.RunStatusServer(address, serverConfig.Port, serverConfig.Label, bufferSize, &ctx, &wg)
			case config.Echoing:
				go server.RunEchoingServer(address, &serverConfig, bufferSize, &ctx, &wg)
			default:
				logging.Error.Printf("Unsupported server type: %s\n", serverConfig.Type)
			}
		}
	}

	// Sleep forever (or until manually stopped)
	<-ctx.Done()
	logging.Info.Println("Shuting down")
	wg.Wait()
	logging.Info.Println("Shut down")
}

func getBufferSize(serverBufferSize int, defaultBufferSize int) int {
	var bufferSize int
	if serverBufferSize == 0 {
		bufferSize = defaultBufferSize
	} else {
		bufferSize = serverBufferSize
	}
	return bufferSize
}
