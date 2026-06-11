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
	SafeMin     float64 // in Unit (bytes for memory params)
	SafeMax     float64
	VersionNote string
}

// gucDocs is the curated knowledge base — the "manual prior". Ranges are
// conservative safe bounds, not hard limits, so the validator rejects
// clearly-dangerous values while leaving the LLM room to tune.
var gucDocs = map[string]GUCDoc{
	"work_mem": {
		Description: "Memory per sort/hash operation, per node, per connection.",
		Guidance:    "Raising it avoids disk spills but is multiplied by concurrent operations; size against max_connections.",
		Unit:        "bytes", SafeMin: 4 << 20, SafeMax: 2 << 30, // 4MB..2GB
	},
	"maintenance_work_mem": {
		Description: "Memory for VACUUM, CREATE INDEX, ALTER TABLE ADD FK.",
		Guidance:    "Larger speeds up maintenance; only a few run at once, so it can exceed work_mem.",
		Unit:        "bytes", SafeMin: 64 << 20, SafeMax: 8 << 30, // 64MB..8GB
	},
	"shared_buffers": {
		Description: "Shared memory for caching data pages.",
		Guidance:    "Commonly ~25% of RAM; beyond ~40% returns diminish as the OS cache also caches pages.",
		Unit:        "bytes", SafeMin: 128 << 20, SafeMax: 256 << 30, // 128MB..256GB
	},
	"effective_cache_size": {
		Description: "Planner's estimate of total cache available (shared_buffers + OS cache). Not an allocation.",
		Guidance:    "Set to ~50-75% of RAM; higher favors index scans.",
		Unit:        "bytes", SafeMin: 256 << 20, SafeMax: 1 << 40, // 256MB..1TB
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
		Unit:        "bytes", SafeMin: 512 << 20, SafeMax: 64 << 30, // 512MB..64GB
	},
	"wal_buffers": {
		Description: "Shared memory for WAL not yet written to disk.",
		Guidance:    "Auto (-1) = 1/32 of shared_buffers is usually fine; 16MB is a common explicit value.",
		Unit:        "bytes", SafeMin: 1 << 20, SafeMax: 1 << 30, // 1MB..1GB
	},
	"effective_io_concurrency": {
		Description: "Number of concurrent I/O operations the planner expects the storage to handle.",
		Guidance:    "Higher (100-200) for SSD/NVMe to enable more aggressive prefetch.",
		Unit:        "count", SafeMin: 0, SafeMax: 1000,
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
		return fmt.Sprintf("%.2f..%.2f", d.SafeMin, d.SafeMax)
	default:
		return fmt.Sprintf("%.0f..%.0f", d.SafeMin, d.SafeMax)
	}
}

// ValidateGUCValue reports whether a proposed value is within the
// documented safe range for a known GUC. Unknown GUCs pass (ok=true) —
// the validator only gates parameters it has a documented opinion on.
func ValidateGUCValue(guc, value string) (ok bool, reason string) {
	doc, known := gucDocs[strings.ToLower(strings.TrimSpace(guc))]
	if !known {
		return true, ""
	}
	v, perr := parseGUCValue(value, doc.Unit)
	if perr != nil {
		return false, fmt.Sprintf("unparseable value %q for %s", value, guc)
	}
	if v < doc.SafeMin || v > doc.SafeMax {
		return false, fmt.Sprintf(
			"%s=%s is outside the safe range %s", guc, value, rangeString(doc))
	}
	return true, ""
}

// parseGUCValue parses a GUC value into a comparable number. Memory units
// (kB/MB/GB/TB) resolve to bytes; ratios/counts to their plain number.
func parseGUCValue(value, unit string) (float64, error) {
	s := strings.TrimSpace(strings.Trim(value, "'\""))
	if unit == "bytes" {
		return parseMemoryToBytes(s)
	}
	return strconv.ParseFloat(s, 64)
}

func parseMemoryToBytes(s string) (float64, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, "TB"):
		mult, s = 1<<40, s[:len(s)-2]
	case strings.HasSuffix(upper, "GB"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		mult, s = 1<<20, s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		mult, s = 1<<10, s[:len(s)-2]
	case strings.HasSuffix(upper, "B"):
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// ValidateConfigSQL extracts the GUC and value from an
// "ALTER SYSTEM SET <guc> = <value>" statement and validates the value
// against the documented safe range (A3). Non-config SQL passes through.
func ValidateConfigSQL(sql string) (ok bool, reason string) {
	guc, value, found := parseAlterSystemSet(sql)
	if !found {
		return true, ""
	}
	return ValidateGUCValue(guc, value)
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
