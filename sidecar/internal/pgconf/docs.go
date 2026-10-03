package pgconf

import "fmt"

// GUCDoc is a curated documentation prior for a tunable PostgreSQL
// parameter (A3). It grounds the LLM's config recommendations in the
// documented semantics and a safe range, GPTuner-style: the LLM proposes
// within the documented region, and ValidateValue gates the result.
type GUCDoc struct {
	Description string
	Guidance    string
	Unit        string  // "bytes", "ratio", "count", "ms"
	SafeMin     float64 // in Unit (bytes for memory, ms for time)
	SafeMax     float64
	// BaseUnit is the PostgreSQL base unit a unitless value is expressed
	// in (pg_settings.unit): "kB", "8kB", "MB" for memory, "ms" or "s" for
	// time. PostgreSQL never reads a bare memory number as bytes (G3-B01).
	BaseUnit string
	// Sentinels are special values PostgreSQL accepts outside the range
	// (e.g. -1 = auto-tune / inherit).
	Sentinels   []float64
	VersionNote string
}

// Docs is the curated knowledge base, the "manual prior", and the list of
// GUCs the advisor may propose as executable SQL. Ranges are conservative
// safe bounds, not hard limits, so the validator rejects clearly-dangerous
// values while leaving the LLM room to tune. A documented GUC that needs a
// restart is still executable, but only with operator approval.
var Docs = map[string]GUCDoc{
	"work_mem": {
		Description: "Memory per sort/hash operation, per node, per connection.",
		Guidance: "Raising it avoids disk spills but is multiplied by concurrent " +
			"operations; size against max_connections.",
		Unit: "bytes", BaseUnit: "kB", SafeMin: 4 << 20, SafeMax: 2 << 30,
	},
	"maintenance_work_mem": {
		Description: "Memory for VACUUM, CREATE INDEX, ALTER TABLE ADD FK.",
		Guidance: "Larger speeds up maintenance; only a few run at once, so it can " +
			"exceed work_mem.",
		Unit: "bytes", BaseUnit: "kB", SafeMin: 64 << 20, SafeMax: 8 << 30,
	},
	"shared_buffers": {
		Description: "Shared memory for caching data pages. Requires a restart.",
		Guidance: "Commonly ~25% of RAM; beyond ~40% returns diminish as the OS " +
			"cache also caches pages.",
		Unit: "bytes", BaseUnit: "8kB", SafeMin: 128 << 20, SafeMax: 256 << 30,
	},
	"effective_cache_size": {
		Description: "Planner's estimate of total cache available (shared_buffers + " +
			"OS cache). Not an allocation.",
		Guidance: "Set to ~50-75% of RAM; higher favors index scans.",
		Unit:     "bytes", BaseUnit: "8kB", SafeMin: 256 << 20, SafeMax: 1 << 40,
	},
	"random_page_cost": {
		Description: "Planner cost of a non-sequential page fetch relative to " +
			"seq_page_cost (1.0).",
		Guidance: "Lower (1.1) for SSD/cloud storage; 4.0 default assumes spinning disk.",
		Unit:     "ratio", SafeMin: 1.0, SafeMax: 4.0,
	},
	"checkpoint_completion_target": {
		Description: "Fraction of the checkpoint interval over which to spread " +
			"checkpoint I/O.",
		Guidance: "0.9 smooths I/O spikes; default is 0.9 on PG14+.",
		Unit:     "ratio", SafeMin: 0.5, SafeMax: 0.95,
	},
	"checkpoint_timeout": {
		Description: "Maximum time between automatic WAL checkpoints.",
		Guidance: "Longer reduces checkpoint I/O and full-page writes at the cost " +
			"of recovery time; 15-30min is common for write-heavy systems.",
		Unit: "ms", BaseUnit: "s", SafeMin: 60_000, SafeMax: 3_600_000,
	},
	"default_statistics_target": {
		Description: "Default number of histogram buckets / most-common-values for " +
			"ANALYZE.",
		Guidance: "Raise (250-500) for columns with skewed data to improve row estimates.",
		Unit:     "count", SafeMin: 100, SafeMax: 1000,
	},
	"max_wal_size": {
		Description: "Soft upper bound on WAL between automatic checkpoints.",
		Guidance: "Larger reduces checkpoint frequency for write-heavy workloads at " +
			"the cost of recovery time.",
		Unit: "bytes", BaseUnit: "MB", SafeMin: 512 << 20, SafeMax: 64 << 30,
	},
	"min_wal_size": {
		Description: "WAL kept for reuse instead of removed after a checkpoint.",
		Guidance:    "Raise to absorb write bursts without creating new WAL segments.",
		Unit:        "bytes", BaseUnit: "MB", SafeMin: 32 << 20, SafeMax: 32 << 30,
	},
	"wal_buffers": {
		Description: "Shared memory for WAL not yet written to disk. Requires a restart.",
		Guidance: "Auto (-1) = 1/32 of shared_buffers is usually fine; 16MB is a " +
			"common explicit value.",
		Unit: "bytes", BaseUnit: "8kB", SafeMin: 1 << 20, SafeMax: 1 << 30,
		Sentinels: []float64{-1},
	},
	"effective_io_concurrency": {
		Description: "Number of concurrent I/O operations the planner expects the " +
			"storage to handle.",
		Guidance: "Higher (100-200) for SSD/NVMe to enable more aggressive prefetch.",
		Unit:     "count", SafeMin: 0, SafeMax: 1000,
	},
	// Limits below were ported from the deleted ValidateConfigRecommendation
	// (G3-D08); the autovacuum ranges also gate the per-table reloptions.
	"max_connections": {
		Description: "Maximum concurrent connections. Requires a restart.",
		Guidance:    "Prefer a connection pooler over raising this; each backend costs memory.",
		Unit:        "count", SafeMin: 10, SafeMax: 10000,
	},
	"autovacuum_max_workers": {
		Description: "Maximum concurrent autovacuum workers. Requires a restart " +
			"before PG18.",
		Guidance: "Raise (5-8) when many tables need vacuum at once; workers share " +
			"autovacuum_vacuum_cost_limit.",
		Unit: "count", SafeMin: 1, SafeMax: 20,
	},
	"autovacuum_naptime": {
		Description: "Delay between autovacuum runs on any given database.",
		Guidance:    "Lower (15-30s) for many busy databases; 1min default.",
		Unit:        "ms", BaseUnit: "s", SafeMin: 5_000, SafeMax: 600_000,
	},
	"autovacuum_vacuum_scale_factor": {
		Description: "Fraction of the table that must be dead before autovacuum runs.",
		Guidance:    "0.01-0.05 for large, write-heavy tables; never 0.",
		Unit:        "ratio", SafeMin: 0.001, SafeMax: 1.0,
	},
	"autovacuum_vacuum_threshold": {
		Description: "Minimum dead tuples before autovacuum runs.",
		Guidance:    "Combine with scale_factor; large values delay vacuum on small tables.",
		Unit:        "count", SafeMin: 0, SafeMax: 1000000,
	},
	"autovacuum_vacuum_insert_scale_factor": {
		Description: "Fraction of inserted tuples that triggers an insert vacuum.",
		Guidance:    "Lower for large append-only tables so visibility maps stay current.",
		Unit:        "ratio", SafeMin: 0.001, SafeMax: 1.0,
	},
	"autovacuum_vacuum_insert_threshold": {
		Description: "Minimum inserted tuples before an insert vacuum runs.",
		Guidance:    "-1 disables insert-triggered vacuum; keep the default unless append-only.",
		Unit:        "count", SafeMin: 0, SafeMax: 1_000_000_000,
		Sentinels: []float64{-1},
	},
	"autovacuum_analyze_scale_factor": {
		Description: "Fraction of the table that must change before autoanalyze runs.",
		Guidance:    "0.01-0.05 for large tables whose distribution shifts; never 0.",
		Unit:        "ratio", SafeMin: 0.001, SafeMax: 1.0,
	},
	"autovacuum_analyze_threshold": {
		Description: "Minimum changed tuples before autoanalyze runs.",
		Guidance:    "Combine with the analyze scale factor.",
		Unit:        "count", SafeMin: 0, SafeMax: 1000000,
	},
	"autovacuum_vacuum_cost_delay": {
		Description: "Sleep between autovacuum cost-limited batches.",
		Guidance:    "2ms (PG12+ default) is usually right; lower speeds vacuum up.",
		Unit:        "ms", BaseUnit: "ms", SafeMin: 0, SafeMax: 100,
		Sentinels: []float64{-1},
	},
	"autovacuum_vacuum_cost_limit": {
		Description: "Cost budget per autovacuum batch (shared across workers).",
		Guidance:    "Raise (1000-2000) on fast storage when autovacuum falls behind.",
		Unit:        "count", SafeMin: 1, SafeMax: 10000,
		Sentinels: []float64{-1},
	},
}

// RangeString renders a documented safe range in the GUC's units.
func RangeString(d GUCDoc) string {
	switch d.Unit {
	case "bytes":
		return fmt.Sprintf("%s..%s", humanBytes(d.SafeMin), humanBytes(d.SafeMax))
	case "ratio":
		return fmt.Sprintf("%.3g..%.3g", d.SafeMin, d.SafeMax)
	case "ms":
		return fmt.Sprintf("%.0fms..%.0fms", d.SafeMin, d.SafeMax)
	default:
		return fmt.Sprintf("%.0f..%.0f", d.SafeMin, d.SafeMax)
	}
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%gTB", b/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%gGB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%gMB", b/(1<<20))
	default:
		return fmt.Sprintf("%gkB", b/(1<<10))
	}
}
