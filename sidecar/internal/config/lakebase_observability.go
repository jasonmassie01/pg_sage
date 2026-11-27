package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

// LakebaseObservabilityConfig reads asynchronous system-table history. It is
// opt-in and restart-bound; it never supplies a live maintenance admission gate.
type LakebaseObservabilityConfig struct {
	Enabled              bool                           `yaml:"enabled" doc:"Read explicitly bound Lakebase system-table history through a SQL warehouse. Default off; never authorizes live host admission."`
	WarehouseID          string                         `yaml:"warehouse_id" doc:"Existing SQL warehouse for bounded system-table reads. Uses Databricks unified authentication, never an inline credential."`
	AllowPayload         bool                           `yaml:"allow_payload" doc:"Permit approved resource-scoped query/plan/log payload reads. Default metadata-only; does not grant UC or unmasking privileges."`
	PollIntervalSeconds  int                            `yaml:"poll_interval_seconds" doc:"Seconds between bounded polls; 0=60, range 30-3600. Queries consume warehouse capacity; the warehouse auto-stops only if this exceeds its idle time (docs/lakebase.md#cost-model)."`
	QueryTimeoutSeconds  int                            `yaml:"query_timeout_seconds" doc:"Per-query deadline; 0=30, range 1-120."`
	MaxRows              int                            `yaml:"max_rows" doc:"Maximum rows per read window; 0=500, range 1-10000. Full windows split rather than silently truncate."`
	MaxBytes             int                            `yaml:"max_bytes" doc:"Maximum result bytes per read; 0=4194304, range 1024-16777216."`
	QueriesPerPoll       int                            `yaml:"queries_per_poll" doc:"Query budget per poll; 0=2x streams+4 (16 for the six default streams), range 1-1000. Forward reads come first; backlog or expiry gaps remain visible."`
	WindowSeconds        int                            `yaml:"window_seconds" doc:"Seconds in the width of each forward and reconcile read, and how far back the first forward read starts; reads narrow when full, widen back, reconcile 4 windows. 0=900, max 7 days."`
	RetentionHours       int                            `yaml:"retention_hours" doc:"Raw provider payload retention by event time; 0=168, range 1-168. Dedup receipts outlive the replay horizon."`
	SettleLagSeconds     int                            `yaml:"settle_lag_seconds" doc:"Seconds forward reads stay behind now so rows the provider commits late are read; 0=120, range 1-3600."`
	ShortLookbackSeconds int                            `yaml:"short_lookback_seconds" doc:"Seconds behind the forward edge each short reconcile sweep re-reads for late rows; 0=7200, range 1-604800."`
	Streams              []string                       `yaml:"streams" doc:"Optional fixed system-table streams. Empty selects plans, logs, ASH, DDL and database counters/gauges."`
	Bindings             []LakebaseObservabilityBinding `yaml:"bindings" doc:"Explicit host/database to endpoint resource bindings; verified against live provider metadata at startup. An omitted endpoint is taken from the SAGE_LAKEBASE_ENDPOINT(S) mapping for the host. No account-wide discovery."`
}

type LakebaseObservabilityBinding struct {
	Host         string `yaml:"host"`
	Database     string `yaml:"database"`
	Endpoint     string `yaml:"endpoint"`
	AllowCompute bool   `yaml:"allow_compute"`
}

// valid reports a direct hostname (never an IP, URL or credential), a
// database name and a well-formed endpoint resource.
func (b LakebaseObservabilityBinding) valid() bool {
	host := b.Host != "" && len(b.Host) <= 253 &&
		!strings.ContainsAny(b.Host, "/@?#:\\ \t\n\r") && net.ParseIP(b.Host) == nil
	database := b.Database != "" && len(b.Database) <= 63 &&
		!strings.ContainsAny(b.Database, "\x00\r\n")
	return host && database && lakebaseid.Valid(b.Endpoint)
}

// resolveAndValidate fills omitted binding endpoints, then validates.
func (c *LakebaseObservabilityConfig) resolveAndValidate(getenv func(string) string) error {
	if err := c.inheritEndpoints(getenv); err != nil {
		return err
	}
	return c.validate()
}

// inheritEndpoints fills each binding that omits endpoint from the
// SAGE_LAKEBASE_ENDPOINT(S) mapping OAuth uses for that host, so native and
// YAML deployments need not state the host-to-endpoint mapping twice.
func (c *LakebaseObservabilityConfig) inheritEndpoints(getenv func(string) string) error {
	for i := range c.Bindings {
		b := &c.Bindings[i]
		if b.Endpoint != "" {
			continue
		}
		endpoint, err := lakebaseid.EndpointForHost(getenv, b.Host)
		if err != nil {
			return fmt.Errorf("lakebase_observability binding for %s: %w", b.Host, err)
		}
		if endpoint == "" {
			return fmt.Errorf("lakebase_observability binding for %s has no endpoint "+
				"and SAGE_LAKEBASE_ENDPOINT(S) maps none", b.Host)
		}
		b.Endpoint = endpoint
	}
	return nil
}

func (c LakebaseObservabilityConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds == 0 {
		return time.Minute
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

func (c LakebaseObservabilityConfig) Binding(host, database string) (LakebaseObservabilityBinding, bool) {
	for _, b := range c.Bindings {
		if strings.EqualFold(strings.TrimSuffix(b.Host, "."), strings.TrimSuffix(host, ".")) && b.Database == database {
			return b, true
		}
	}
	return LakebaseObservabilityBinding{}, false
}

func (c LakebaseObservabilityConfig) validate() error {
	for _, b := range []struct {
		name        string
		v, min, max int
	}{
		{"poll_interval_seconds", c.PollIntervalSeconds, 30, 3600},
		{"query_timeout_seconds", c.QueryTimeoutSeconds, 1, 120},
		{"max_rows", c.MaxRows, 1, 10000}, {"max_bytes", c.MaxBytes, 1024, 16 << 20},
		{"queries_per_poll", c.QueriesPerPoll, 1, 1000}, {"window_seconds", c.WindowSeconds, 1, 604800},
		{"retention_hours", c.RetentionHours, 1, 168},
		{"settle_lag_seconds", c.SettleLagSeconds, 1, 3600},
		{"short_lookback_seconds", c.ShortLookbackSeconds, 1, 604800},
	} {
		if b.v != 0 && (b.v < b.min || b.v > b.max) {
			return fmt.Errorf("lakebase_observability.%s outside supported bounds", b.name)
		}
	}
	if c.Enabled && (c.WarehouseID == "" || len(c.Bindings) == 0) {
		return fmt.Errorf("lakebase_observability requires an existing warehouse and explicit endpoint/database bindings")
	}
	if c.WarehouseID != "" && !regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`).MatchString(c.WarehouseID) {
		return fmt.Errorf("lakebase_observability.warehouse_id is invalid")
	}
	seen := map[string]bool{}
	for _, b := range c.Bindings {
		if !b.valid() {
			return fmt.Errorf("lakebase_observability requires a direct hostname, " +
				"database and endpoint resource")
		}
		key := strings.ToLower(strings.TrimSuffix(b.Host, ".")) + "/" + b.Database
		if seen[key] {
			return fmt.Errorf("lakebase_observability has duplicate database bindings")
		}
		seen[key] = true
	}
	allowed := map[string]bool{"plan_history": true, "postgres_logs": true, "active_session_history": true, "ddl_history": true, "compute_counters": true, "compute_gauges": true, "database_counters": true, "database_gauges": true, "wait_event_counters": true, "pg_stat_statements_counters": true}
	seen = map[string]bool{}
	for _, s := range c.Streams {
		if !allowed[s] || seen[s] {
			return fmt.Errorf("lakebase_observability stream invalid or repeated")
		}
		seen[s] = true
	}
	return nil
}
