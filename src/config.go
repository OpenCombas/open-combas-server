package main

type ServerConfig struct {
	WorldPort        int    `json:"WorldPort" env:"WorldPort"`
	WorldOldPort     int    `json:"WorldOtherPort" env:"WorldOtherPort"`
	ServerStatusPort int    `json:"ServerStatusPort" env:"ServerStatusPort"`
	ListeningAddress string `json:"ListeningAddress" env:"ListeningAddress"`
	BufferSize       int    `json:"BufferSize" env:"BufferSize"`
}

func LoadConfig() ServerConfig {
	cfg := ServerConfig{
		WorldPort:        1215,
		WorldOldPort:     1255,
		ServerStatusPort: 1207,
		ListeningAddress: "0.0.0.0",
		BufferSize:       4000,
	}
	//todo: parse config file, for now it is hardcoded

	return cfg
}
