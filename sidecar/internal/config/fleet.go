package config

import "fmt"

// DatabaseConfig describes a single database in fleet mode.
type DatabaseConfig struct {
	Name                     string   `yaml:"name" doc:"Logical name for this database in fleet mode. Must be unique across the fleet." mode:"fleet-only"`
	Host                     string   `yaml:"host" doc:"Hostname or IP of this fleet database." mode:"fleet-only"`
	Port                     int      `yaml:"port" doc:"TCP port of this fleet database." mode:"fleet-only"`
	User                     string   `yaml:"user" doc:"Login role the sidecar uses to connect to this fleet database." mode:"fleet-only"`
	Password                 string   `yaml:"password" doc:"Password for the login role. Encrypted at rest using encryption_key when persisted to the meta database." mode:"fleet-only" secret:"true"`
	Database                 string   `yaml:"database" doc:"Database name on the target instance." mode:"fleet-only"`
	SSLMode                  string   `yaml:"sslmode" doc:"libpq sslmode for this connection (disable, prefer, require, verify-ca, verify-full)." mode:"fleet-only"`
	MaxConnections           int      `yaml:"max_connections" doc:"Maximum connections the sidecar pgx pool opens to this database. Falls back to defaults.max_connections when zero." mode:"fleet-only"`
	Tags                     []string `yaml:"tags" doc:"Free-form tags used to group databases for fleet operations (e.g. tier=prod, team=payments)." mode:"fleet-only"`
	TrustLevel               string   `yaml:"trust_level" doc:"Per-database override of the global trust.level. Useful for running a pilot database in autonomous while the fleet stays in advisory." mode:"fleet-only"`
	ExecutionMode            string   `yaml:"execution_mode" doc:"Per-database action execution mode: auto executes eligible actions, approval queues them for review, manual disables autonomous queueing. Defaults to defaults.execution_mode or auto." mode:"fleet-only"`
	ExecutorEnabled          *bool    `yaml:"executor_enabled" doc:"Per-database executor override. Nil defaults to enabled; set false to disable action execution on this database specifically." mode:"fleet-only"`
	LLMEnabled               *bool    `yaml:"llm_enabled" doc:"Per-database LLM override. Nil defaults to enabled; set false to skip LLM calls for this database." mode:"fleet-only"`
	CollectorIntervalSeconds int      `yaml:"collector_interval_seconds" doc:"Per-database collector interval override. Zero falls back to defaults.collector_interval_seconds." mode:"fleet-only"`
	AnalyzerIntervalSeconds  int      `yaml:"analyzer_interval_seconds" doc:"Per-database analyzer interval override. Zero falls back to defaults.analyzer_interval_seconds." mode:"fleet-only"`
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
	MaxConnections           int    `yaml:"max_connections" doc:"Default max_connections applied to fleet databases that don't set their own value." mode:"fleet-only"`
	TrustLevel               string `yaml:"trust_level" doc:"Default trust level applied to fleet databases. Falls back to the global trust.level when unset." mode:"fleet-only"`
	ExecutionMode            string `yaml:"execution_mode" doc:"Default action execution mode applied to fleet databases that don't set their own value. One of auto, approval, manual." mode:"fleet-only"`
	CollectorIntervalSeconds int    `yaml:"collector_interval_seconds" doc:"Default collector interval applied to fleet databases that don't set their own value." mode:"fleet-only"`
	AnalyzerIntervalSeconds  int    `yaml:"analyzer_interval_seconds" doc:"Default analyzer interval applied to fleet databases that don't set their own value." mode:"fleet-only"`
}

// APIConfig configures the REST API + dashboard server.
type APIConfig struct {
	ListenAddr string `yaml:"listen_addr" doc:"Address the REST API and dashboard HTTP server binds to (host:port). Use :8080 to listen on all interfaces."`

	// TrustedProxies lists IP addresses (or CIDR blocks) whose
	// X-Forwarded-For header may be trusted for determining the real
	// client IP used by the login rate limiter. Any proxy not listed
	// here has its XFF header ignored, so a direct attacker cannot
	// bypass per-IP rate limits by spoofing the header. Defaults to
	// ["127.0.0.1", "::1"] when unset. Expand this list when running
	// behind a reverse proxy, load balancer, or CDN.
	TrustedProxies []string `yaml:"trusted_proxies" doc:"IPs or CIDR blocks permitted to set X-Forwarded-For. Defaults to loopback only. Expand when running behind a trusted reverse proxy, load balancer, or CDN." warning:"Adding an untrusted or externally-reachable proxy lets attackers spoof their client IP and bypass login rate limits."`
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
			if d.TrustLevel == "" && c.Trust.Level != "" {
				d.TrustLevel = c.Trust.Level
			}
			if d.ExecutionMode == "" && c.Defaults.ExecutionMode != "" {
				d.ExecutionMode = c.Defaults.ExecutionMode
			}
			if d.ExecutionMode == "" {
				d.ExecutionMode = "auto"
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
