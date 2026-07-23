package config

import "fmt"

type PolicyConfig struct {
	Profile string `yaml:"profile" doc:"Bootstrap standing-policy profile: staffed or unattended."`
}

type ValueConfig struct {
	ToilModelVersion int `yaml:"toil_model_version" doc:"Active calibrated toil-model version."`
}

type VerifyConfig struct {
	WindowMinutes    int     `yaml:"window_minutes" doc:"Initial verification window."`
	WindowMaxMinutes int     `yaml:"window_max_minutes" doc:"Maximum verification window."`
	MinGainPct       float64 `yaml:"min_gain_pct" doc:"Minimum gain required to retain a change."`
	RegressPct       float64 `yaml:"regress_pct" doc:"Regression percentage that triggers revert."`
	WriteImpactPct   float64 `yaml:"write_impact_pct" doc:"Maximum write-latency increase."`
	MinSamples       int     `yaml:"min_samples" doc:"Minimum verification samples."`
}

type CloneProviderConfig struct {
	Provider           string `yaml:"provider" doc:"Clone provider: none, dle, or snapshot."`
	DLEEndpoint        string `yaml:"dle_endpoint" doc:"Database Lab Engine API endpoint."`
	DLEToken           string `yaml:"dle_token" doc:"Database Lab bearer token." secret:"true"`
	MaxCloneAgeMinutes int    `yaml:"max_clone_age_minutes" doc:"Maximum source snapshot age."`
}

type CustodianConfig struct {
	Freeze FreezeCustodianConfig `yaml:"freeze"`
	WAL    WALCustodianConfig    `yaml:"wal"`
}

type FreezeCustodianConfig struct {
	RedBufferPct float64 `yaml:"red_buffer_pct" doc:"Remaining XID percentage classified red."`
}

type WALCustodianConfig struct {
	AbandonAfterMinutes       int     `yaml:"abandon_after_minutes" doc:"Inactive abandonment age."`
	RetainedWALDiskPctCeiling float64 `yaml:"retained_wal_disk_pct_ceiling" doc:"WAL retention limit."`
}

type MCPConfig struct {
	Enabled   bool   `yaml:"enabled" doc:"Enable the intent-level MCP server."`
	Transport string `yaml:"transport" doc:"MCP transport: stdio or http."`
}

func (c *Config) validateAgentNative() error {
	if c.Policy.Profile != "staffed" && c.Policy.Profile != "unattended" {
		return fmt.Errorf("policy.profile must be staffed or unattended")
	}
	if c.Value.ToilModelVersion <= 0 {
		return fmt.Errorf("value.toil_model_version must be positive")
	}
	if c.Verify.WindowMinutes <= 0 ||
		c.Verify.WindowMaxMinutes < c.Verify.WindowMinutes {
		return fmt.Errorf("verify window must be positive and within hard maximum")
	}
	if c.Verify.MinSamples <= 0 {
		return fmt.Errorf("verify.min_samples must be positive")
	}
	if c.Verify.MinGainPct < 0 || c.Verify.RegressPct < 0 ||
		c.Verify.WriteImpactPct < 0 {
		return fmt.Errorf("verify percentages must be non-negative")
	}
	if c.Clone.Provider != "none" && c.Clone.Provider != "dle" &&
		c.Clone.Provider != "snapshot" {
		return fmt.Errorf("clone.provider must be none, dle, or snapshot")
	}
	if c.Clone.Provider == "dle" && c.Clone.DLEEndpoint == "" {
		return fmt.Errorf("clone.dle_endpoint is required for dle provider")
	}
	if c.Clone.MaxCloneAgeMinutes <= 0 {
		return fmt.Errorf("clone.max_clone_age_minutes must be positive")
	}
	if c.Custodian.Freeze.RedBufferPct <= 0 ||
		c.Custodian.Freeze.RedBufferPct > 100 {
		return fmt.Errorf("custodian.freeze.red_buffer_pct must be within 1-100")
	}
	if c.Custodian.WAL.AbandonAfterMinutes <= 0 ||
		c.Custodian.WAL.RetainedWALDiskPctCeiling <= 0 ||
		c.Custodian.WAL.RetainedWALDiskPctCeiling > 100 {
		return fmt.Errorf("custodian.wal limits are invalid")
	}
	if c.MCP.Transport != "stdio" && c.MCP.Transport != "http" {
		return fmt.Errorf("mcp.transport must be stdio or http")
	}
	return nil
}
