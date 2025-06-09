package config

import (
	"ChromehoundsStatusServer/logging"
	"os"

	"github.com/BurntSushi/toml"
)

// Definition for config of the server itself. Which address to bind to, what is default buffer size, and what services to run.
type Config struct {
	ListeningAddress  string
	DefaultBufferSize int
	Servers           []ServerConfig
	Logging           LoggingConfig
}

// Definition of configuration for specific service running at a port.
// if buffersize is left at 0, it will use default value.
type ServerConfig struct {
	Label   string
	Port    int
	Enabled bool
	Type    ServerType
}

type LoggingConfig struct {
	EnablePerformanceMonitoring bool
	PerformanceReportInterval   int
	Verbose                     bool
}

type ServerType string

const (
	Echoing ServerType = "Echoing"
	Status  ServerType = "Status"
)

var configFilename = "config.toml"

func LoadConfig() Config {
	var conf Config
	f, err := os.Open(configFilename)
	if err != nil {
		conf = generateDefaultConfig()
		if os.IsNotExist(err) {
			logging.Info.Printf("[CONFIG] config file does not exist. generating")
			f, err := os.Create(configFilename)
			if err != nil {
				logging.Error.Printf("[CONFIG] failed writing config to file!")
			} else {
				encoder := toml.NewEncoder(f)
				encoder.Encode(conf)
			}
		} else {
			logging.Error.Printf("[CONFIG] error opening config file: %s", err)
		}
		logging.Warn.Printf("[CONFIG] fallback to default")
	} else {
		defer f.Close()
		if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
			logging.Warn.Printf("[CONFIG] failed decoding config - fallback to default")
			conf = generateDefaultConfig()
		}
	}
	return conf
}

func generateDefaultConfig() Config {
	return Config{
		ListeningAddress:  "0.0.0.0",
		DefaultBufferSize: 4000,
		Servers: []ServerConfig{
			{
				Label:   "WORLD",
				Port:    1215,
				Enabled: true,
				Type:    Echoing,
			},
			{
				Label:   "WORLD_OLD",
				Port:    1255,
				Enabled: true,
				Type:    Echoing,
			},
			{
				Label:   "STATUS",
				Port:    1207,
				Enabled: true,
				Type:    Status,
			},
		},
	}
}
