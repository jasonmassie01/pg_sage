package advisor

import (
	"fmt"
	"strconv"
	"strings"
)

// GUCDoc is a curated documentation prior for a tunable PostgreSQL
// parameter (A3). It grounds the LLM's config recommendations in the
// documented semantics and a safe range, GPTuner-style: the LLM proposes
// within the documented region, and ValidateGUCValue gates the result.
type GUCDoc struct {
	Description string
	Guidance    string
	Unit        string  // "bytes", "ratio", "count", "ms"
	SafeMin     float64 // in Unit (bytes for memory, ms for time)
	SafeMax     float64
	// BaseUnit is the PostgreSQL base unit a unitless value is expressed
	// in (pg_settings.unit): "kB", "8kB", "MB" for memory, "ms" for time.
	// PostgreSQL never reads a bare memory number as bytes (G3-B01).
	BaseUnit string
	// Sentinels are special values PostgreSQL accepts outside the range
	// (e.g. -1 = auto-tune / inherit).
	Sentinels   []float64
	VersionNote string
}

// gucDocs is the curated knowledge base — the "manual prior". Ranges are
// conservative safe bounds, not hard limits, so the validator rejects
// clearly-dangerous values while leaving the LLM room to tune.
var gucDocs = map[string]GUCDoc{
	"work_mem": {
		Description: "Memory per sort/hash operation, per node, per connection.",
		Guidance:    "Raising it avoids disk spills but is multiplied by concurrent operations; size against max_connections.",
		Unit:        "bytes", BaseUnit: "kB", SafeMin: 4 << 20, SafeMax: 2 << 30,
	},
	"maintenance_work_mem": {
		Description: "Memory for VACUUM, CREATE INDEX, ALTER TABLE ADD FK.",
		Guidance:    "Larger speeds up maintenance; only a few run at once, so it can exceed work_mem.",
		Unit:        "bytes", BaseUnit: "kB", SafeMin: 64 << 20, SafeMax: 8 << 30,
	},
	"shared_buffers": {
		Description: "Shared memory for caching data pages. Requires a restart.",
		Guidance:    "Commonly ~25% of RAM; beyond ~40% returns diminish as the OS cache also caches pages.",
		Unit:        "bytes", BaseUnit: "8kB", SafeMin: 128 << 20, SafeMax: 256 << 30,
	},
	"effective_cache_size": {
		Description: "Planner's estimate of total cache available (shared_buffers + OS cache). Not an allocation.",
		Guidance:    "Set to ~50-75% of RAM; higher favors index scans.",
		Unit:        "bytes", BaseUnit: "8kB", SafeMin: 256 << 20, SafeMax: 1 << 40,
	},
	"random_page_cost": {
		Description: "Planner cost of a non-sequential page fetch relative to seq_page_cost (1.0).",
		Guidance:    "Lower (1.1) for SSD/cloud storage; 4.0 default assumes spinning disk.",
		Unit:        "ratio", SafeMin: 1.0, SafeMax: 4.0,
	},
	"checkpoint_completion_target": {
		Description: "Fraction of the checkpoint interval over which to spread checkpoint I/O.",
		Guidance:    "0.9 smooths I/O spikes; default is 0.9 on PG14+.",
		Unit:        "ratio", SafeMin: 0.5, SafeMax: 0.95,
	},
	"default_statistics_target": {
		Description: "Default number of histogram buckets / most-common-values for ANALYZE.",
		Guidance:    "Raise (250-500) for columns with skewed data to improve row estimates.",
		Unit:        "count", SafeMin: 100, SafeMax: 1000,
	},
	"max_wal_size": {
		Description: "Soft upper bound on WAL between automatic checkpoints.",
		Guidance:    "Larger reduces checkpoint frequency for write-heavy workloads at the cost of recovery time.",
		Unit:        "bytes", BaseUnit: "MB", SafeMin: 512 << 20, SafeMax: 64 << 30,
	},
	"wal_buffers": {
		Description: "Shared memory for WAL not yet written to disk. Requires a restart.",
		Guidance:    "Auto (-1) = 1/32 of shared_buffers is usually fine; 16MB is a common explicit value.",
		Unit:        "bytes", BaseUnit: "8kB", SafeMin: 1 << 20, SafeMax: 1 << 30,
		Sentinels: []float64{-1},
	},
	"effective_io_concurrency": {
		Description: "Number of concurrent I/O operations the planner expects the storage to handle.",
		Guidance:    "Higher (100-200) for SSD/NVMe to enable more aggressive prefetch.",
		Unit:        "count", SafeMin: 0, SafeMax: 1000,
	},
	// Limits below were ported from the deleted ValidateConfigRecommendation
	// (G3-D08); they also gate ALTER TABLE ... SET (reloption) values.
	"max_connections": {
		Description: "Maximum concurrent connections. Requires a restart.",
		Guidance:    "Prefer a connection pooler over raising this; each backend costs memory.",
		Unit:        "count", SafeMin: 10, SafeMax: 10000,
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

// DocContext returns the documentation prior for the named GUCs, to be
// embedded in an LLM prompt. Unknown GUCs are skipped.
func DocContext(pgVersion int, gucs ...string) string {
	var b strings.Builder
	b.WriteString("PostgreSQL parameter documentation (authoritative — base your recommendation on this):\n")
	for _, g := range gucs {
		doc, ok := gucDocs[g]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s %s Safe range: %s. ",
			g, doc.Description, doc.Guidance, rangeString(doc))
		if doc.BaseUnit != "" {
			fmt.Fprintf(&b, "A value without a unit is in %s. ", doc.BaseUnit)
		}
		if doc.VersionNote != "" {
			fmt.Fprintf(&b, "Version note: %s ", doc.VersionNote)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func rangeString(d GUCDoc) string {
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

// ValidateGUCValue reports whether a proposed value is within the
// documented safe range for a known GUC. Unknown GUCs pass (ok=true) —
// the validator only gates parameters it has a documented opinion on.
// Unitless values are read in the GUC's PostgreSQL base unit.
func ValidateGUCValue(guc, value string) (ok bool, reason string) {
	doc, known := gucDocs[strings.ToLower(strings.TrimSpace(guc))]
	if !known {
		return true, ""
	}
	if isSentinel(value, doc) {
		return true, ""
	}
	v, perr := parseGUCValue(value, doc)
	if perr != nil {
		return false, fmt.Sprintf("unparseable value %q for %s: %v", value, guc, perr)
	}
	if v < doc.SafeMin || v > doc.SafeMax {
		return false, fmt.Sprintf(
			"%s=%s is outside the safe range %s", guc, value, rangeString(doc))
	}
	return true, ""
}

// isSentinel reports whether a unitless value is one of the GUC's
// special values (compared before base-unit scaling).
func isSentinel(value string, doc GUCDoc) bool {
	raw := strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `'"`))
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return false
	}
	for _, sentinel := range doc.Sentinels {
		if n == sentinel {
			return true
		}
	}
	return false
}

// ValidateConfigSQL validates the value(s) of an
// "ALTER SYSTEM SET <guc> = <value>" statement or an
// "ALTER TABLE <t> SET (<reloption> = <value>, ...)" statement against
// the documented safe ranges (A3). Other SQL passes through.
func ValidateConfigSQL(sql string) (ok bool, reason string) {
	if opts, found := parseAlterTableReloptions(sql); found {
		for _, kv := range opts {
			if ok, reason := ValidateGUCValue(kv[0], kv[1]); !ok {
				return false, reason
			}
		}
		return true, ""
	}
	guc, value, found := parseAlterSystemSet(sql)
	if !found {
		return true, ""
	}
	return ValidateGUCValue(guc, value)
}

// parseAlterTableReloptions extracts key/value pairs from
// "ALTER TABLE <t> SET (k = v, ...)" so per-table autovacuum overrides
// are validated with the same documented ranges as ALTER SYSTEM.
func parseAlterTableReloptions(sql string) ([][2]string, bool) {
	low := strings.ToLower(strings.TrimSpace(sql))
	if !strings.HasPrefix(low, "alter table ") {
		return nil, false
	}
	open := strings.Index(low, " set (")
	end := strings.LastIndex(low, ")")
	if open < 0 || end <= open {
		return nil, false
	}
	body := strings.TrimSpace(sql)[open+len(" set ("):end]
	var out [][2]string
	for _, part := range strings.Split(body, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out = append(out, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return out, true
}

func parseAlterSystemSet(sql string) (guc, value string, found bool) {
	low := strings.ToLower(sql)
	const marker = "alter system set "
	i := strings.Index(low, marker)
	if i < 0 {
		return "", "", false
	}
	rest := strings.TrimSpace(sql[i+len(marker):])
	if eq := strings.Index(rest, "="); eq >= 0 {
		guc = strings.TrimSpace(rest[:eq])
		value = strings.TrimSpace(rest[eq+1:])
	} else if to := strings.Index(strings.ToLower(rest), " to "); to >= 0 {
		guc = strings.TrimSpace(rest[:to])
		value = strings.TrimSpace(rest[to+4:])
	} else {
		return "", "", false
	}
	value = strings.TrimSpace(strings.TrimRight(value, ";"))
	return guc, value, guc != "" && value != ""
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
