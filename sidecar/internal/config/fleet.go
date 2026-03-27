package config

import "fmt"

// DatabaseConfig describes a single database in fleet mode.
type DatabaseConfig struct {
	Name                     string   `yaml:"name"`
	Host                     string   `yaml:"host"`
	Port                     int      `yaml:"port"`
	User                     string   `yaml:"user"`
	Password                 string   `yaml:"password"`
	Database                 string   `yaml:"database"`
	SSLMode                  string   `yaml:"sslmode"`
	MaxConnections           int      `yaml:"max_connections"`
	Tags                     []string `yaml:"tags"`
	TrustLevel               string   `yaml:"trust_level"`
	ExecutorEnabled          *bool    `yaml:"executor_enabled"`
	LLMEnabled               *bool    `yaml:"llm_enabled"`
	CollectorIntervalSeconds int      `yaml:"collector_interval_seconds"`
	AnalyzerIntervalSeconds  int      `yaml:"analyzer_interval_seconds"`
}

// ConnString builds a postgres:// URL from the config fields.
func (d DatabaseConfig) ConnString() string {
	sslMode := d.SSLMode
	if sslMode == "" {
		sslMode = "prefer"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Database, sslMode)
}

// HasTag reports whether this database has the given tag.
func (d DatabaseConfig) HasTag(tag string) bool {
	for _, t := range d.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// IsExecutorEnabled returns whether the executor is enabled.
// Defaults to true if not explicitly set.
func (d DatabaseConfig) IsExecutorEnabled() bool {
	if d.ExecutorEnabled == nil {
		return true
	}
	return *d.ExecutorEnabled
}

// IsLLMEnabled returns whether LLM features are enabled.
// Defaults to true if not explicitly set.
func (d DatabaseConfig) IsLLMEnabled() bool {
	if d.LLMEnabled == nil {
		return true
	}
	return *d.LLMEnabled
}

// DefaultsConfig holds default values applied to fleet databases
// that don't override them.
type DefaultsConfig struct {
	MaxConnections           int    `yaml:"max_connections"`
	TrustLevel               string `yaml:"trust_level"`
	CollectorIntervalSeconds int    `yaml:"collector_interval_seconds"`
	AnalyzerIntervalSeconds  int    `yaml:"analyzer_interval_seconds"`
}

// APIConfig configures the REST API + dashboard server.
type APIConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}

// normalize fills in defaults and synthesizes Databases from legacy
// Postgres fields when appropriate. Must be called after loading
// config but before validate().
func (c *Config) normalize() {
	if c.Mode == "" {
		c.Mode = "extension"
	}

	// Standalone with legacy Postgres config: synthesize Databases[0].
	if c.Mode == "standalone" && len(c.Databases) == 0 && c.Postgres.Host != "" {
		name := c.Postgres.Database
		if name == "" {
			name = "default"
		}
		c.Databases = []DatabaseConfig{{
			Name:           name,
			Host:           c.Postgres.Host,
			Port:           c.Postgres.Port,
			User:           c.Postgres.User,
			Password:       c.Postgres.Password,
			Database:       c.Postgres.Database,
			SSLMode:        c.Postgres.SSLMode,
			MaxConnections: c.Postgres.MaxConnections,
		}}
	}

	// Fleet mode: apply DefaultsConfig to databases with zero-valued fields.
	if c.Mode == "fleet" {
		for i := range c.Databases {
			d := &c.Databases[i]
			if d.MaxConnections == 0 && c.Defaults.MaxConnections != 0 {
				d.MaxConnections = c.Defaults.MaxConnections
			}
			if d.TrustLevel == "" && c.Defaults.TrustLevel != "" {
				d.TrustLevel = c.Defaults.TrustLevel
			}
			if d.CollectorIntervalSeconds == 0 && c.Defaults.CollectorIntervalSeconds != 0 {
				d.CollectorIntervalSeconds = c.Defaults.CollectorIntervalSeconds
			}
			if d.AnalyzerIntervalSeconds == 0 && c.Defaults.AnalyzerIntervalSeconds != 0 {
				d.AnalyzerIntervalSeconds = c.Defaults.AnalyzerIntervalSeconds
			}
		}
	}

	// Set API listen addr if empty.
	if c.API.ListenAddr == "" {
		c.API.ListenAddr = DefaultAPIListenAddr
	}
}
