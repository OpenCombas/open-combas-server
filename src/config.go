package main

type ServerConfig struct {
	ServerStatusPort int                `json:"ServerStatusPort" env:"ServerStatusPort"`
	ListeningAddress string             `json:"ListeningAddress" env:"ListeningAddress"`
	BufferSize       int                `json:"BufferSize" env:"BufferSize"`
	EchoingServers   []EchoServerConfig `json:"EchoingServers" env:"EchoingServers"`
}
type EchoServerConfig struct {
	Label string
	Port  int
}

func LoadConfig() ServerConfig {
	cfg := ServerConfig{
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
	//todo: parse config file, for now it is hardcoded

	return cfg
}
