package main

type ServerConfig struct {
	WorldPort        int32  `json:"WorldPort" env:"WorldPort"`
	WorldOldPort     int32  `json:"WorldOtherPort" env:"WorldOtherPort"`
	ServerStatusPort int32  `json:"ServerStatusPort" env:"ServerStatusPort"`
	ListeningAddress string `json:"ListeningAddress" env:"ListeningAddress"`
}

func LoadConfig() ServerConfig {
	cfg := ServerConfig{
		WorldPort:        1215,
		WorldOldPort:     1255,
		ServerStatusPort: 1207,
		ListeningAddress: "0.0.0.0",
	}
	//todo: parse config file, for now it is hardcoded

	return cfg
}
