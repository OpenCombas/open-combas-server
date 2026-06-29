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
				srv := server.NewStatusServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.World:
				srv := server.NewWorldServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.WorldArea:
				srv := server.NewWorldAreaServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.WorldAreaInfo:
				srv := server.NewWorldAreaInfoServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.WorldMapDetail:
				srv := server.NewWorldMapDetailServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.WorldNews:
				srv := server.NewWorldNewsServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.SquadReg:
				srv := server.NewSquadRegServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.SquadLogin:
				srv := server.NewSquadLoginServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.SquadAck:
				srv := server.NewSquadAckServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.BattleReport:
				srv := server.NewBattleReportServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			case config.Echoing:
				srv := server.NewEchoServer(address, serverConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(serverConfig.Type), "server_name": string(serverConfig.Label)}, reg))
				go srv.Run()
			default:
				logging.Error.Printf("Unsupported server type: %s\n", serverConfig.Type)
			}
		}
	}
	for _, staticMessageServerConfig := range cfg.StaticMessageServers {
		if staticMessageServerConfig.Enabled {
			srv := server.NewStaticMessageServer(address, &staticMessageServerConfig, cfg.DefaultBufferSize, &cfg.Logging, ctx, &wg, cfg.Prometheus, prometheus.WrapRegistererWith(prometheus.Labels{"server_type": string(staticMessageServerConfig.Type), "server_name": string(staticMessageServerConfig.Label)}, reg))
			go srv.Run()
		}
	}

	// Sleep forever (or until manually stopped)
	<-ctx.Done()
	logging.Info.Println("Shuting down")
	wg.Wait()
	logging.Info.Println("Shut down")
}
