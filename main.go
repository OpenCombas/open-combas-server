package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/logging/profiling"
	"ChromehoundsStatusServer/pooling"
	"ChromehoundsStatusServer/server"
	"context"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var wg sync.WaitGroup

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var cfg = config.LoadConfig()
	logging.Info.Println("Config Loaded")

	// Initialize buffer pools for performance
	pooling.InitBufferPools(cfg.DefaultBufferSize)

	// Initialize Prometheus Metrics registry
	reg := prometheus.NewRegistry()
	if cfg.Prometheus.EnableGoProfiling && cfg.Prometheus.Enabled {
		reg.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
	}

	if cfg.Prometheus.Enabled {
		http.Handle(cfg.Prometheus.PrometheusHttpPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		go http.ListenAndServe(cfg.Prometheus.PrometheusListenAddress, nil)
	}

	// Start performance monitoring if enabled
	if cfg.Logging.EnablePerformanceMonitoring {
		profiling.StartGlobalReporting(&cfg.Logging)
		logging.Info.Println("Performance monitoring enabled")
		defer profiling.PrintGlobalStats()
	}

	logging.Info.Println("App started")
	var address = net.ParseIP(cfg.ListeningAddress)
	for _, serverConfig := range cfg.Servers {
		if serverConfig.Enabled {
			switch serverConfig.Type {
			case config.Status:
				go server.RunStatusServer(address, &serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
			case config.Echoing:
				go server.RunEchoingServer(address, &serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
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
