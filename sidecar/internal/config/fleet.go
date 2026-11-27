package config

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// DatabaseConfig describes a single database in fleet mode.
type DatabaseConfig struct {
	connectionOptions        string
	Name                     string   `yaml:"name" doc:"Logical name for this database in fleet mode. Must be unique across the fleet." mode:"fleet-only"`
	Host                     string   `yaml:"host" doc:"Hostname or IP of this fleet database." mode:"fleet-only"`
	Port                     int      `yaml:"port" doc:"TCP port of this fleet database." mode:"fleet-only"`
	User                     string   `yaml:"user" doc:"Login role the sidecar uses to connect to this fleet database." mode:"fleet-only"`
	Password                 string   `yaml:"password" doc:"Password for the login role. Encrypted at rest using encryption_key when persisted to the meta database." mode:"fleet-only" secret:"true"`
	Database                 string   `yaml:"database" doc:"Database name on the target instance." mode:"fleet-only"`
	SSLMode                  string   `yaml:"sslmode" doc:"libpq sslmode for this connection (disable, prefer, require, verify-ca, verify-full)." mode:"fleet-only"`
	ActionDSN                string   `yaml:"action_dsn" doc:"Optional DSN of a second role on the same database. Only executor actions and backend cancel/terminate use it (a lazy pool of at most 7); the main role only observes. Empty: one role." mode:"fleet-only" secret:"true"`
	MaxConnections           int      `yaml:"max_connections" doc:"Maximum connections the sidecar pgx pool opens to this database. Falls back to defaults.max_connections when zero." mode:"fleet-only"`
	Tags                     []string `yaml:"tags" doc:"Free-form tags used to group databases for fleet operations (e.g. tier=prod, team=payments)." mode:"fleet-only"`
	TrustLevel               string   `yaml:"trust_level" doc:"Per-database override of the global trust.level. Useful for running a pilot database in autonomous while the fleet stays in advisory." mode:"fleet-only"`
	TrustLevelExplicit       bool     `yaml:"-"`
	ExecutionMode            string   `yaml:"execution_mode" doc:"Per-database mode: auto follows trust/risk policy; approval queues actions; manual disables background queueing and execution. Trust never promotes manual. Defaults to fleet default or auto." mode:"fleet-only"`
	ExecutorEnabled          *bool    `yaml:"executor_enabled" doc:"Per-database executor hard gate. Nil defaults to enabled; false blocks background queueing/execution and explicit manual mutations on this database." mode:"fleet-only"`
	LLMEnabled               *bool    `yaml:"llm_enabled" doc:"Per-database LLM override. Nil defaults to enabled; set false to skip LLM calls for this database." mode:"fleet-only"`
	CollectorIntervalSeconds int      `yaml:"collector_interval_seconds" doc:"Per-database collector interval override. Zero falls back to defaults.collector_interval_seconds." mode:"fleet-only"`
	AnalyzerIntervalSeconds  int      `yaml:"analyzer_interval_seconds" doc:"Per-database analyzer interval override. Zero falls back to defaults.analyzer_interval_seconds." mode:"fleet-only"`
	// DisabledRules replaces analyzer.disabled_rules for this database
	// when set (market-11); nil inherits the global list.
	DisabledRules []string `yaml:"disabled_rules" doc:"Per-database analyzer.disabled_rules: replaces the global list here when set; [] re-enables every rule." mode:"fleet-only"`

	Provider string `yaml:"provider" doc:"Optional provider override, e.g. rds, cloud-sql, neon or self-managed. Used only while detection cannot tell; a disagreement with detection keeps the detected provider and warns." mode:"fleet-only"`
	// CloudResource names this database's provider resource when its host
	// does not (an IP, the Cloud SQL Auth Proxy, an RDS Proxy, custom DNS).
	CloudResource CloudResourceConfig `yaml:"cloud_resource"`
	// CNPG names this database's CloudNativePG Cluster (market-12).
	CNPG CNPGConfig `yaml:"cnpg"`

	// Verify carries this database's own load-admission attestation (D6).
	Verify DatabaseVerifyConfig `yaml:"verify"`
	// Replicas are the standbys the agent kill switch reaches (agents_kill.go).
	Replicas []DatabaseReplica `yaml:"replicas"`
}

// DatabaseVerifyConfig holds per-database load-admission attestations.
type DatabaseVerifyConfig struct {
	IOCapacity *IOCapacityConfig `yaml:"io_capacity" doc:"Declared IO capacity." mode:"fleet-only"`
}

// ConnString builds a postgres:// URL from the config fields.
func (d DatabaseConfig) ConnString() string {
	sslMode := d.SSLMode
	if sslMode == "" {
		sslMode = "prefer"
	}
	query, _ := url.ParseQuery(d.connectionOptions)
	query.Set("sslmode", sslMode)
	path := "/" + d.Database
	if strings.HasPrefix(d.Database, "/") {
		// pgx strips all leading path slashes; dbname retains the literal name.
		query.Set("dbname", d.Database)
		path = "/"
	}
	u := url.URL{
		Scheme: "postgres", User: url.UserPassword(d.User, d.Password),
		Host: net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path: path, RawQuery: query.Encode(),
	}
	return u.String()
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

	// TrustedOrigins lists browser origins (scheme://host[:port]) whose
	// cross-origin writes the API accepts (apiauth-10). Needed only when a
	// reverse proxy rewrites Host and the browser sends no Sec-Fetch-Site,
	// or when a separately hosted UI calls the API.
	TrustedOrigins []string `yaml:"trusted_origins" doc:"Browser origins (scheme://host[:port]) allowed to send cross-origin writes to the API. Empty by default: only same-origin browser writes and non-browser clients are accepted." warning:"Every listed origin can submit authenticated writes with a signed-in user's cookie; list only origins you control."`

	// AccessLog writes one line per mutating API request and per 401/403
	// (apiauth-12); nil means enabled.
	AccessLog *bool `yaml:"access_log" doc:"Log one structured line per mutating API request and per 401/403 response: method, route pattern, user id, role, status and latency, never bodies or query strings. Default true." warning:"Turning this off removes the record of who changed what through the API."`

	// MigrationLintRatePerMinute bounds POST /api/v1/migrations/lint per
	// signed-in user (migration-11); 0 or less means the default, 30.
	MigrationLintRatePerMinute int `yaml:"migration_lint_rate_per_minute" doc:"Requests per minute each signed-in user may send to POST /api/v1/migrations/lint, which reads catalog statistics on the monitored database. Default 30."`

	// DatabaseGrants turns on per-database authorization in fleet and
	// meta-db modes (apiauth-08); standalone ignores it.
	DatabaseGrants bool `yaml:"database_grants" doc:"Fleet and meta-db modes: enforce per-database grants on operator writes. A user with no grants keeps their global role; one with grants operates only where granted. Default false." warning:"While off, grants are stored but not enforced."`
}

// AccessLogEnabled reports whether the API access log is on (default: on).
func (a APIConfig) AccessLogEnabled() bool { return a.AccessLog == nil || *a.AccessLog }

// DefaultMigrationLintRatePerMinute is api.migration_lint_rate_per_minute
// when unset.
const DefaultMigrationLintRatePerMinute = 30

// MigrationLintRate is the per-user migration lint budget per minute.
func (a APIConfig) MigrationLintRate() int {
	if a.MigrationLintRatePerMinute <= 0 {
		return DefaultMigrationLintRatePerMinute
	}
	return a.MigrationLintRatePerMinute
}

// normalize fills in defaults and synthesizes Databases from legacy
// Postgres fields when appropriate. Must be called after loading
// config but before validate().
func (c *Config) normalize() {
	if c.Mode == "" {
		c.Mode = DefaultMode
	}

	// Standalone with legacy Postgres config: synthesize Databases[0].
	// When a DSN URL is the connection source, its identity (host, db,
	// user) wins over the localhost defaults so the registered instance
	// names the database actually monitored.
	if c.Mode == "standalone" && len(c.Databases) == 0 && c.Postgres.Host != "" {
		c.Databases = []DatabaseConfig{c.standaloneDatabase()}
	}

	// Fleet mode: apply DefaultsConfig to databases with zero-valued fields.
	if c.Mode == "fleet" {
		for i := range c.Databases {
			d := &c.Databases[i]
			if d.TrustLevel != "" {
				d.TrustLevelExplicit = true
			}
			if d.MaxConnections == 0 && c.Defaults.MaxConnections != 0 {
				d.MaxConnections = c.Defaults.MaxConnections
			}
			if d.TrustLevel == "" && c.Defaults.TrustLevel != "" {
				d.TrustLevel = c.Defaults.TrustLevel
				d.TrustLevelExplicit = true
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

// standaloneDatabase is the database the legacy postgres: settings describe.
func (c *Config) standaloneDatabase() DatabaseConfig {
	db := DatabaseConfig{
		Host:           c.Postgres.Host,
		Port:           c.Postgres.Port,
		User:           c.Postgres.User,
		Password:       c.Postgres.Password,
		Database:       c.Postgres.Database,
		SSLMode:        c.Postgres.SSLMode,
		MaxConnections: c.Postgres.MaxConnections,
		Provider:       c.Postgres.Provider,
		CloudResource:  c.Postgres.CloudResource,
		ActionDSN:      c.Postgres.ActionDSN,
		CNPG:           c.Postgres.CNPG,
	}
	applyDatabaseURL(&db, c.Postgres.DatabaseURL)
	db.Name = db.Database
	if db.Name == "" {
		db.Name = "default"
	}
	return db
}

// applyDatabaseURL overlays the identity carried by a postgres:// URL.
// Key/value DSNs and unparsable values leave db unchanged; the runtime
// connects with the raw DSN either way.
func applyDatabaseURL(db *DatabaseConfig, raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return
	}
	if host := u.Hostname(); host != "" {
		db.Host = host
	}
	if port, err := strconv.Atoi(u.Port()); err == nil && port > 0 {
		db.Port = port
	}
	if u.User != nil {
		db.User = u.User.Username()
		if password, ok := u.User.Password(); ok {
			db.Password = password
		}
	}
	if name := strings.TrimPrefix(u.Path, "/"); name != "" {
		db.Database = name
	}
	if mode := u.Query().Get("sslmode"); mode != "" {
		db.SSLMode = mode
	}
}
