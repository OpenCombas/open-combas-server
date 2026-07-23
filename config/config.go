package config

import (
	"ChromehoundsStatusServer/logging"
	"os"

	"github.com/BurntSushi/toml"
)

// Definition for config of the server itself. Which address to bind to, what is default buffer size, and what services to run.
type Config struct {
	ListeningAddress     string
	DefaultBufferSize    int
	Servers              []ServerConfig
	StaticMessageServers []StaticMessageServerConfig
	StatusServers        []StatusServerConfig
	EventServers         []EventServerConfig
	Logging              LoggingConfig
	Prometheus           PrometheusConfig
	Mongo                MongoConfig
}

// MongoConfig points the server at the shared MongoDB instance (the same one Xenia-WebServices uses).
// When Enabled is false the server runs fully on the static in-memory world model (current behaviour).
type MongoConfig struct {
	Enabled  bool
	URI      string
	Database string // must match Xenia's database; its Mongoose URI has no path so it defaults to "test"
}

// Definition of configuration for specific service running at a port.
// if buffersize is left at 0, it will use default value.
type ServerConfig struct {
	Label   string
	Port    int
	Enabled bool
	Type    ServerType
}

type StaticMessageServerConfig struct {
	ServerConfig
	BufferContent string
}

type StatusServerConfig struct {
	ServerConfig
	IsResetting bool
}

// EventServerConfig configures a server in the world-event subsystem: WorldNews (serves the news feed) and
// BattleReport (generates events from mission results). GenerateEvents gates data-driven events -- when
// false, BattleReport records no capture/dissolution events and WorldNews serves only the seeded "War Breaks
// Out" briefing, so the news board never fills with generated stories.
type EventServerConfig struct {
	ServerConfig
	GenerateEvents bool
	// CpuBattleScale multiplies the occupation + renown a real squad earns from a mission against a
	// CPU/AI squad (a non-"TM" opponent), letting an operator tune PvE relative to real PvP -- e.g. 0.5
	// to halve PvE farming, or 2.0 to reward it. It scales BOTH the war-map occupation move and the squad
	// ledger credit together, so every consumer agrees. Only BattleReport reads it. Unset or <= 0 means
	// 1.0 (no scaling) -- so an absent TOML value keeps the current behavior rather than zeroing rewards.
	CpuBattleScale float64
}

type LoggingConfig struct {
	EnablePerformanceMonitoring bool
	PerformanceReportInterval   int
	Verbose                     bool
}

type PrometheusConfig struct {
	Enabled                 bool
	EnableGoProfiling       bool
	PrometheusListenAddress string
	PrometheusHttpPath      string
}

type ServerType string

const (
	Echoing        ServerType = "Echoing"
	Status         ServerType = "Status"
	World          ServerType = "World"
	WorldArea      ServerType = "WorldArea"
	WorldAreaInfo  ServerType = "WorldAreaInfo"
	WorldMapDetail ServerType = "WorldMapDetail"
	WorldNews      ServerType = "WorldNews"
	WorldDonation  ServerType = "WorldDonation"
	SquadReg       ServerType = "SquadReg"
	SquadJoin      ServerType = "SquadJoin"
	SquadLogin     ServerType = "SquadLogin"
	SquadAck       ServerType = "SquadAck"
	SquadConfig    ServerType = "SquadConfig"
	SquadWithdraw  ServerType = "SquadWithdraw"
	SquadMemberNum ServerType = "SquadMemberNum"
	SquadEmblem    ServerType = "SquadEmblem"
	SquadRanking   ServerType = "SquadRanking"
	SquadHistory   ServerType = "SquadHistory"
	BattleReport   ServerType = "BattleReport"
	Shop           ServerType = "Shop"
	Buffer         ServerType = "Buffer"
	OrderedMessage ServerType = "OrderedMessage"
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
	applyEnvOverrides(&conf)
	validateAndFix(&conf)

	return conf
}

// applyEnvOverrides lets the container/orchestrator inject deployment-specific values without baking a
// host-specific config.toml into the image. Used by docker-compose to point the server at the shared
// MongoDB service. Setting MONGO_URI also implies Mongo is enabled.
func applyEnvOverrides(config *Config) {
	if v := os.Getenv("MONGO_URI"); v != "" {
		config.Mongo.URI = v
		config.Mongo.Enabled = true
	}
	if v := os.Getenv("MONGO_DATABASE"); v != "" {
		config.Mongo.Database = v
	}
	if v := os.Getenv("LISTENING_ADDRESS"); v != "" {
		config.ListeningAddress = v
	}
}

func validateAndFix(config *Config) {
	if config.Logging.PerformanceReportInterval <= 0 {
		logging.Warn.Printf("[CONFIG] impossible value for reporting interval: %ds, fallback to 10s", config.Logging.PerformanceReportInterval)
		config.Logging.PerformanceReportInterval = 10
	}

	if len(config.Servers) == 0 {
		logging.Warn.Printf("[CONFIG] No servers declared!")
	}

	if config.Mongo.Enabled {
		if config.Mongo.URI == "" {
			logging.Warn.Printf("[CONFIG] Mongo enabled with empty URI, falling back to mongodb://127.0.0.1:27017/")
			config.Mongo.URI = "mongodb://127.0.0.1:27017/"
		}
		if config.Mongo.Database == "" {
			logging.Warn.Printf("[CONFIG] Mongo enabled with empty Database, falling back to \"test\"")
			config.Mongo.Database = "test"
		}
	}
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
			{
				Label:   "SQUAD_LOGIN_OLD",
				Port:    1204,
				Enabled: true,
				Type:    Echoing,
			},
			{
				Label:   "SQUAD_LOGIN",
				Port:    1244,
				Enabled: true,
				Type:    Echoing,
			},
			{
				Label:   "SQUAD_NEWS",
				Port:    1216,
				Enabled: true,
				Type:    Echoing,
			},
			{
				Label:   "WORLD_DONATION",
				Port:    1266,
				Enabled: true,
				Type:    WorldDonation,
			},
		},
		// World-event servers. These MUST live here (not in Servers) so GenerateEvents/CpuBattleScale
		// decode and main.go's EventServers loop dispatches them. GenerateEvents enables data-driven
		// news/capture events; CpuBattleScale (BattleReport only) scales occ+renown earned vs a CPU
		// opponent (1.0 = no scaling).
		EventServers: []EventServerConfig{
			{
				ServerConfig:   ServerConfig{Label: "BATTLE_REPORT_DEBUG", Port: 1214, Enabled: true, Type: BattleReport},
				GenerateEvents: true,
				CpuBattleScale: 1.0,
			},
			{
				ServerConfig:   ServerConfig{Label: "BATTLE_REPORT", Port: 1254, Enabled: true, Type: BattleReport},
				GenerateEvents: true,
				CpuBattleScale: 1.0,
			},
			{
				ServerConfig:   ServerConfig{Label: "WORLD_NEWS_DEBUG", Port: 1212, Enabled: true, Type: WorldNews},
				GenerateEvents: true,
			},
			{
				ServerConfig:   ServerConfig{Label: "WORLD_NEWS", Port: 1252, Enabled: true, Type: WorldNews},
				GenerateEvents: true,
			},
		},
		StaticMessageServers: []StaticMessageServerConfig{
			{
				ServerConfig: ServerConfig{
					Label:   "SQUAD_REG",
					Port:    1241,
					Enabled: true,
					Type:    Buffer,
				},
				BufferContent: "434800003030303930303030344539324241444400000000",
			},
			{
				ServerConfig: ServerConfig{
					Label:   "SQUAD_REG_OLD",
					Port:    1201,
					Enabled: true,
					Type:    Buffer,
				},
				BufferContent: "434800003030303930303030344539324241444400000000",
			},
		},
		Logging: LoggingConfig{
			Verbose:                     false,
			PerformanceReportInterval:   10,
			EnablePerformanceMonitoring: true,
		},
		Prometheus: PrometheusConfig{
			Enabled:                 true,
			EnableGoProfiling:       true,
			PrometheusListenAddress: "0.0.0.0:9090",
			PrometheusHttpPath:      "/metrics",
		},
		Mongo: MongoConfig{
			Enabled:  false,
			URI:      "mongodb://127.0.0.1:27017/",
			Database: "test",
		},
	}
}
