package main

import (
	"os"

	"github.com/BurntSushi/toml"
)

type ServerConfig struct {
	ServerStatusPort int
	ListeningAddress string
	BufferSize       int
	EchoingServers   []EchoServerConfig
}
type EchoServerConfig struct {
	Label string
	Port  int
}

var configFilename = "config.toml"

func LoadConfig() ServerConfig {
	var conf ServerConfig
	f, err := os.Open(configFilename)
	if err != nil {
		conf = generateDefaultConfig()
		if os.IsNotExist(err) {
			Info.Printf("[CONFIG] config file does not exist. generating")
			f, err := os.Create(configFilename)
			if err != nil {
				Error.Printf("[CONFIG] failed writing config to file!")
			} else {
				encoder := toml.NewEncoder(f)
				encoder.Encode(conf)
			}
		} else {
			Error.Printf("[CONFIG] error opening config file: %w", err)
		}
		Warn.Printf("[CONFIG] fallback to default")
	} else {
		defer f.Close()
		if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
			Warn.Printf("[CONFIG] failed decoding config - fallback to default")
			conf = generateDefaultConfig()
		}
	}
	return conf
}

func generateDefaultConfig() ServerConfig {
	return ServerConfig{
		ServerStatusPort: 1207,
		ListeningAddress: "0.0.0.0",
		BufferSize:       4000,
		EchoingServers: []EchoServerConfig{
			{
				Label: "WORLD",
				Port:  1215,
			},
			{
				Label: "WORLD_OLD",
				Port:  1255,
			},
		},
	}
}
