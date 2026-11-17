package config

import (
	"fmt"
	"time"
)

type PolicyConfig struct {
	Profile string `yaml:"profile" doc:"Bootstrap standing-policy profile: staffed or unattended." api:"policy_profile" enum:"staffed|unattended"`
}

type ValueConfig struct {
	ToilModelVersion int `yaml:"toil_model_version" doc:"Active calibrated toil-model version." api:"int_pos"`
}

type VerifyConfig struct {
	WindowMinutes    int     `yaml:"window_minutes" doc:"Initial verification window." api:"int_pos"`
	WindowMaxMinutes int     `yaml:"window_max_minutes" doc:"Maximum verification window." api:"int_pos"`
	MinGainPct       float64 `yaml:"min_gain_pct" doc:"Minimum gain required to retain a change." api:"float_nonneg"`
	RegressPct       float64 `yaml:"regress_pct" doc:"Regression percentage that triggers revert." api:"float_nonneg"`
	WriteImpactPct   float64 `yaml:"write_impact_pct" doc:"Table WAL-per-tuple rise that reverts." api:"float_nonneg"`
	MinSamples       int     `yaml:"min_samples" doc:"Minimum verification samples." api:"int_pos"`
	// Method and OnExpired are verification v2's switches (lab-01c).
	Method    string `yaml:"method" doc:"Verifier: v1, shadow (v2 beside v1, act on v1), v2, or auto (v2 where the latest back-test passed within 35 days, else shadow). Default: auto." warning:"v2 decides reverts from blocks per call against untouched control statements; auto switches to it only after a passing back-test."`
	OnExpired string `yaml:"on_expired" doc:"A monitor whose window ended unwatched: ask (queue a Revert action N item), keep (keep the change, recorded) or revert (operator rollback). Default: ask."`
	// The verifier back-test (lab-02) that auto's switch reads.
	BacktestMaxRuntimeMinutes int `yaml:"backtest_max_runtime_minutes" doc:"Longest one verifier back-test may run, 1-1440 minutes; a run past it stops and stores nothing. Default: 10."`
	BacktestPlacebos          int `yaml:"backtest_placebos" doc:"Placebo steps the weekly verifier back-test draws, 200-10000 (auto needs 200 usable). Default: 300."`

	// IO load admission (D6). io_capacity is standalone-only; fleet
	// databases declare it in databases[].verify.io_capacity.
	IOBaselineDays  int               `yaml:"io_baseline_days" doc:"IO baseline days; 0 disables."`
	DropWindowHours int               `yaml:"drop_window_hours" doc:"Hours an index drop is verified for (its business cycle), 1-8760. The dropped index definition is kept and re-created on the first miss inside the window. Default: 168 (7 days)." warning:"A shorter window can miss a weekly or monthly job that needs the index; the sidecar warns at startup."`
	IOBaselineHours int               `yaml:"io_baseline_hours" doc:"IO baseline hours, 1-8760; when set it takes precedence over io_baseline_days (even 0). 0: use io_baseline_days." warning:"An hour-scale baseline admits index builds on a short load history; the sidecar warns at startup."`
	IOSampleDays    int               `yaml:"io_sample_retention_days" doc:"IO sample retention days."`
	IOCapacity      *IOCapacityConfig `yaml:"io_capacity" doc:"Declared IO capacity (standalone)."`
}

// IOCapacityConfig is an operator attestation of provisioned throughput,
// in mebibytes per second.
type IOCapacityConfig struct {
	ReadWriteMBps float64 `yaml:"read_write_mbps" doc:"Provisioned data read+write MiB/s."`
	WALMBps       float64 `yaml:"wal_mbps" doc:"Provisioned WAL write MiB/s."`
}

type CloneProviderConfig struct {
	Provider           string `yaml:"provider" doc:"Clone provider: none, dle (Database Lab), lakebase_branch or neon_branch (a branch per rehearsal), aurora_clone (an Aurora clone) or rds_restore (an RDS restore: a full copy, needs allow_full_copy)." api:"clone_provider" enum:"none|dle|lakebase_branch|neon_branch|aurora_clone|rds_restore"`
	DLEEndpoint        string `yaml:"dle_endpoint" doc:"Database Lab Engine API endpoint." api:"string"`
	DLEToken           string `yaml:"dle_token" doc:"Database Lab bearer token." secret:"true" api:"string"`
	MaxCloneAgeMinutes int    `yaml:"max_clone_age_minutes" doc:"Maximum source snapshot age." api:"int_pos"`
	// RehearsalsPerHour caps the index rehearsals one database starts in
	// any hour, tuning agent and index floor together (lab-04a).
	RehearsalsPerHour int `yaml:"rehearsals_per_hour" doc:"Most index rehearsals on a clone one database starts in any hour, 1 to 60. Default: 2."`
	// RehearsalStatementTimeoutSeconds bounds each statement a rehearsal
	// executes on the clone (EXPLAIN ANALYZE).
	RehearsalStatementTimeoutSeconds int `yaml:"rehearsal_statement_timeout_seconds" doc:"Longest one statement an index rehearsal executes on the clone may run, in seconds, 1 to 300; a statement that hits it records timed out, not a cost. Default: 5."`
	// AllowFullCopy opts in to full-copy clones (a whole-instance restore:
	// hours and a full instance bill), lab-05a.
	AllowFullCopy bool `yaml:"allow_full_copy" doc:"Allow full-copy clones that restore a whole instance (the *_restore and cloudsql_clone providers, or a provider that does not describe its clones). Default: false: they create nothing."`
	// MaxCreatesPerHour caps the clones one deployment creates in any
	// hour, all purposes together (lab-05a).
	MaxCreatesPerHour int `yaml:"max_creates_per_hour" doc:"Most clones this deployment creates in any hour, rehearsals and every other purpose together, 1 to 60. Default: 4."`
	// Lakebase configures clone.provider lakebase_branch (market-03).
	Lakebase CloneLakebaseConfig `yaml:"lakebase"`
	// Neon configures clone.provider neon_branch (lab-05b).
	Neon CloneNeonConfig `yaml:"neon"`
	// Aurora configures clone.provider aurora_clone (lab-05c).
	Aurora CloneAuroraConfig `yaml:"aurora"`
	// RestoreDeadlineMinutes bounds a full-copy restore (lab-05d).
	RestoreDeadlineMinutes int `yaml:"restore_deadline_minutes" doc:"Longest a full-copy restore (rds_restore) may take to become available, in minutes, 30 to 720; one still not ready is left to the reaper. Default: 120."`
	// RDS configures clone.provider rds_restore (lab-05d).
	RDS CloneRDSConfig `yaml:"rds"`
}

// CloneLakebaseConfig is the Lakebase project rehearsal branches are made
// in (clone.provider lakebase_branch).
type CloneLakebaseConfig struct {
	Project       string  `yaml:"project" doc:"Lakebase project id rehearsal branches are created in; the principal needs CAN_MANAGE on it."`
	SourceBranch  string  `yaml:"source_branch" doc:"Branch id (or projects/<project>/branches/<id>) a rehearsal branches from. Default: the monitored endpoint's branch."`
	TTLMinutes    int     `yaml:"ttl_minutes" doc:"Server-side expiry of every rehearsal branch, in minutes, 1 to 1440: the backstop if pg_sage cannot delete it. Default: 60."`
	EndpointMaxCU float64 `yaml:"endpoint_max_cu" doc:"Autoscaling maximum, in compute units, of the read-write endpoint on a rehearsal branch. Default: 1."`
}

type CustodianConfig struct {
	Freeze FreezeCustodianConfig `yaml:"freeze"`
	WAL    WALCustodianConfig    `yaml:"wal"`
}

type FreezeCustodianConfig struct {
	RedBufferPct float64 `yaml:"red_buffer_pct" doc:"Remaining XID percentage classified red." api:"float_pct_open"`
}

type WALCustodianConfig struct {
	AbandonAfterMinutes       int     `yaml:"abandon_after_minutes" doc:"Inactive abandonment age." api:"int_pos"`
	RetainedWALDiskPctCeiling float64 `yaml:"retained_wal_disk_pct_ceiling" doc:"WAL retention limit." api:"float_pct_pos"`
}

type MCPConfig struct {
	Enabled   bool   `yaml:"enabled" doc:"Enable the intent-level MCP server." api:"bool"`
	Transport string `yaml:"transport" doc:"MCP transport: http (default, credential required) or stdio." api:"mcp_transport" enum:"stdio|http"`
	// OAuth makes MCP over HTTP an OAuth 2.1 resource server (mcp_oauth.go).
	OAuth MCPOAuthConfig `yaml:"oauth"`
	// StdioPrincipal names the agent principal the stdio client acts as
	// (agents_core.go).
	StdioPrincipal string `yaml:"stdio_principal" doc:"Name of the agent principal the stdio client acts as. Empty: stdio keeps read and propose tools capped at approval, and agent_* tools answer agent_unsponsored. Default: empty."`
	// CallTimeoutSeconds bounds one stdio tools/call (mcp-04).
	CallTimeoutSeconds int `yaml:"call_timeout_seconds" doc:"Seconds one stdio tools/call may run before it is cancelled and answered -32800. 5 to 600. Default: 90."`
}

// CallTimeout is mcp.call_timeout_seconds as a duration; an unset (zero)
// value keeps the default, so a call is never unbounded.
func (c MCPConfig) CallTimeout() time.Duration {
	if c.CallTimeoutSeconds <= 0 {
		return DefaultMCPCallTimeoutSeconds * time.Second
	}
	return time.Duration(c.CallTimeoutSeconds) * time.Second
}

func (c *Config) validateAgentNative() error {
	if err := ValidStdioPrincipal(c.MCP.StdioPrincipal); err != nil {
		return err
	}
	if c.Policy.Profile != "staffed" && c.Policy.Profile != "unattended" {
		return fmt.Errorf("policy.profile must be staffed or unattended")
	}
	if c.Value.ToilModelVersion <= 0 {
		return fmt.Errorf("value.toil_model_version must be positive")
	}
	if c.Verify.WindowMinutes <= 0 ||
		c.Verify.WindowMaxMinutes < c.Verify.WindowMinutes {
		return fmt.Errorf("verify window: verify.window_minutes must be positive and at most "+
			"verify.window_max_minutes, got %d and %d",
			c.Verify.WindowMinutes, c.Verify.WindowMaxMinutes)
	}
	if c.Verify.MinSamples <= 0 {
		return fmt.Errorf("verify.min_samples must be positive")
	}
	if err := c.Verify.validatePercentages(); err != nil {
		return err
	}
	if err := c.Verify.validateMethod(); err != nil {
		return err
	}
	if err := c.Verify.validateBacktest(); err != nil {
		return err
	}
	if err := c.Clone.validate(); err != nil {
		return err
	}
	// The open interval freeze.BufferThresholds uses as given; written so
	// NaN, which compares false with both bounds, is refused (vacuum-11b).
	if red := c.Custodian.Freeze.RedBufferPct; !(red > 0 && red < 100) {
		return fmt.Errorf("custodian.freeze.red_buffer_pct must be greater than 0 " +
			"and less than 100")
	}
	if err := c.Custodian.WAL.validate(); err != nil {
		return err
	}
	if c.MCP.Transport != "stdio" && c.MCP.Transport != "http" {
		return fmt.Errorf("mcp.transport must be stdio or http")
	}
	if c.MCP.CallTimeoutSeconds < MinMCPCallTimeoutSeconds ||
		c.MCP.CallTimeoutSeconds > MaxMCPCallTimeoutSeconds {
		return fmt.Errorf("mcp.call_timeout_seconds must be within 5-600, got %d",
			c.MCP.CallTimeoutSeconds)
	}
	return nil
}

// validatePercentages names the first verify percentage below zero.
func (v VerifyConfig) validatePercentages() error {
	for _, p := range []struct {
		key   string
		value float64
	}{
		{"verify.min_gain_pct", v.MinGainPct},
		{"verify.regress_pct", v.RegressPct},
		{"verify.write_impact_pct", v.WriteImpactPct},
	} {
		if p.value < 0 {
			return fmt.Errorf("verify percentages: %s must be non-negative, got %v",
				p.key, p.value)
		}
	}
	return nil
}

// validate names the custodian.wal limit that is out of range.
func (w WALCustodianConfig) validate() error {
	if w.AbandonAfterMinutes <= 0 {
		return fmt.Errorf("custodian.wal.abandon_after_minutes must be positive, got %d",
			w.AbandonAfterMinutes)
	}
	if w.RetainedWALDiskPctCeiling <= 0 || w.RetainedWALDiskPctCeiling > 100 {
		return fmt.Errorf("custodian.wal.retained_wal_disk_pct_ceiling must be above 0 "+
			"and at most 100, got %v", w.RetainedWALDiskPctCeiling)
	}
	return nil
}
