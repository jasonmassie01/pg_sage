package config

import "fmt"

// SelfBudgetConfig is pg_sage's declared budget for itself (roadmap
// phase 3): what the sidecar may cost the host and the database it
// watches. Each resource is measured continuously, exported as metrics
// and, when exceeded, raised as one sage_self_budget finding naming the
// top consumers. 0 disables a resource.
type SelfBudgetConfig struct {
	CPUMsPerCycle   int   `yaml:"cpu_ms_per_cycle" doc:"CPU time the sidecar process may use per collector cycle, in ms (all monitored databases together). 0 disables. Range 0-3600000. Default: 600 (1% of one core at the default 60 s interval)."`
	DBTimeMsPerHour int   `yaml:"db_time_ms_per_hour" doc:"Database time pg_sage's own statements may use per hour on each database, in ms. 0 keeps analyzer.self_cost_budget_ms (per collector cycle) as the database-time budget. Range 0-3600000. Default: 0."`
	BlocksPerHour   int64 `yaml:"blocks_per_hour" doc:"Shared buffer blocks (hit or read, 8 KB each) pg_sage's own statements may touch per hour on each database. 0 disables. Range 0-1000000000000. Default: 18000000."`
	StorageMB       int   `yaml:"storage_mb" doc:"Size the sage schema (tables, TOAST and indexes) may reach on each database, in MB. 0 disables. Range 0-10485760. Default: 10240."`
}

// Self-budget defaults: 1% of one core per default 60 s cycle; 18M blocks
// per hour is 5,000 8 KB blocks a second (mostly cache hits); 10 GB of
// sage schema.
const (
	DefaultSelfBudgetCPUMsPerCycle = 600
	DefaultSelfBudgetBlocksPerHour = 18_000_000
	DefaultSelfBudgetStorageMB     = 10240
)

// Self-budget bounds.
const (
	maxSelfBudgetMs        = 3_600_000
	maxSelfBudgetBlocks    = 1_000_000_000_000
	maxSelfBudgetStorageMB = 10 * 1024 * 1024
)

// DefaultSelfBudget is the shipped budget.
func DefaultSelfBudget() SelfBudgetConfig {
	return SelfBudgetConfig{CPUMsPerCycle: DefaultSelfBudgetCPUMsPerCycle,
		BlocksPerHour: DefaultSelfBudgetBlocksPerHour, StorageMB: DefaultSelfBudgetStorageMB}
}

// validate refuses a value outside its range.
func (s SelfBudgetConfig) validate() error {
	for _, f := range []struct {
		key      string
		v, limit int64
	}{
		{"cpu_ms_per_cycle", int64(s.CPUMsPerCycle), maxSelfBudgetMs},
		{"db_time_ms_per_hour", int64(s.DBTimeMsPerHour), maxSelfBudgetMs},
		{"blocks_per_hour", s.BlocksPerHour, maxSelfBudgetBlocks},
		{"storage_mb", int64(s.StorageMB), maxSelfBudgetStorageMB},
	} {
		if f.v < 0 || f.v > f.limit {
			return fmt.Errorf("self_budget.%s must be 0-%d, got %d", f.key, f.limit, f.v)
		}
	}
	return nil
}

// EffectiveSelfDBTimeMsPerHour is the database-time budget per hour and
// whether self_budget sets it: the explicit value, else
// analyzer.self_cost_budget_ms per collector cycle scaled to an hour
// (raised as sage_self_cost, so self_budget does not check it again).
func (c *Config) EffectiveSelfDBTimeMsPerHour() (float64, bool) {
	if c.SelfBudget.DBTimeMsPerHour > 0 {
		return float64(c.SelfBudget.DBTimeMsPerHour), true
	}
	secs := c.Collector.IntervalSeconds
	if secs <= 0 {
		secs = 60
	}
	return float64(c.Analyzer.SelfCostBudgetMs) * 3600 / float64(secs), false
}
