package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
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
	var cfg = config.LoadConfig()

	// Initialize buffer pools for performance
	pooling.InitBufferPools(cfg.DefaultBufferSize)

	// Start performance monitoring if enabled
	if cfg.Logging.EnablePerformanceMonitoring {
		profiling.StartGlobalReporting(&cfg.Logging)
		logging.Info.Println("Performance monitoring enabled")
		defer profiling.PrintGlobalStats()
	}
	logging.Info.Println("App started")
	logging.Info.Println("Config Loaded")
	var address = net.ParseIP(cfg.ListeningAddress)
	for _, serverConfig := range cfg.Servers {
		if serverConfig.Enabled {
			switch serverConfig.Type {
			case config.Status:
				go server.RunStatusServer(address, &serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg)
			case config.Echoing:
				go server.RunEchoingServer(address, &serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg)
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
