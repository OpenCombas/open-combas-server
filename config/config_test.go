package config

import "testing"

// TestMongoDefaultsWhenEnabledEmpty verifies validateAndFix backfills URI/Database when Mongo is
// enabled but left blank, so an operator only has to set Enabled = true to get sane defaults.
func TestMongoDefaultsWhenEnabledEmpty(t *testing.T) {
	c := Config{Mongo: MongoConfig{Enabled: true}}
	c.Logging.PerformanceReportInterval = 10
	validateAndFix(&c)

	if c.Mongo.URI == "" {
		t.Error("expected URI to be defaulted when Mongo is enabled")
	}
	if c.Mongo.Database != "test" {
		t.Errorf("expected Database default \"test\", got %q", c.Mongo.Database)
	}
}

// TestEnvOverrides verifies the container-facing env vars override config values and that MONGO_URI
// implies Mongo is enabled (so docker-compose can wire the connection without a custom config.toml).
func TestEnvOverrides(t *testing.T) {
	t.Setenv("MONGO_URI", "mongodb://mongo:27017/")
	t.Setenv("MONGO_DATABASE", "opencombas")
	t.Setenv("LISTENING_ADDRESS", "0.0.0.0")

	c := Config{}
	applyEnvOverrides(&c)

	if !c.Mongo.Enabled {
		t.Error("expected MONGO_URI to enable Mongo")
	}
	if c.Mongo.URI != "mongodb://mongo:27017/" {
		t.Errorf("MONGO_URI override = %q", c.Mongo.URI)
	}
	if c.Mongo.Database != "opencombas" {
		t.Errorf("MONGO_DATABASE override = %q", c.Mongo.Database)
	}
	if c.ListeningAddress != "0.0.0.0" {
		t.Errorf("LISTENING_ADDRESS override = %q", c.ListeningAddress)
	}
}

// TestMongoUntouchedWhenDisabled verifies a disabled Mongo block is left exactly as-is.
func TestMongoUntouchedWhenDisabled(t *testing.T) {
	c := Config{Mongo: MongoConfig{Enabled: false}}
	c.Logging.PerformanceReportInterval = 10
	validateAndFix(&c)

	if c.Mongo.URI != "" || c.Mongo.Database != "" {
		t.Errorf("disabled Mongo config should not be defaulted, got URI=%q Database=%q", c.Mongo.URI, c.Mongo.Database)
	}
}
