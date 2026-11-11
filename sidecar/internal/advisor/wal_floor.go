package advisor

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// pgDefaultMaxWALSize is what RESET max_wal_size returns to (1GB).
const pgDefaultMaxWALSize = 1 << 30

// walCurrent is the server's max_wal_size in bytes; unknown when the
// setting is missing or unreadable, never 0.
type walCurrent struct {
	bytes float64
	known bool
}

func currentMaxWALSize(settings []collector.PGSetting) walCurrent {
	for _, s := range settings {
		if s.Name != "max_wal_size" {
			continue
		}
		v, err := pgconf.ParseValue(s.Setting, pgconf.GUCDoc{Unit: "bytes", BaseUnit: s.Unit})
		if err != nil || v <= 0 {
			return walCurrent{}
		}
		return walCurrent{bytes: v, known: true}
	}
	return walCurrent{}
}

// maxWALSizeTarget is the bytes a statement sets max_wal_size to (a RESET
// returns to the default); NaN for an unparseable value.
func maxWALSizeTarget(sql string) (float64, bool) {
	st, ok := pgconf.ParseAlterSystem(sql)
	if !ok || st.Name != "max_wal_size" {
		return 0, false
	}
	if st.Reset {
		return pgDefaultMaxWALSize, true
	}
	v, err := pgconf.ParseValue(st.Value, pgconf.Docs["max_wal_size"])
	if err != nil {
		return math.NaN(), true
	}
	return v, true
}

// filterUnsafeMaxWALSize is the deterministic backstop behind the prompt's
// sizing rule (advisors-05). A max_wal_size below the current value or the
// floor is dropped, and the floor finding replaces it when the floor is a
// raise. Without a floor a value cannot be shown safe and stays advice; so
// does a raise above walCeilingFactor x the floor.
func filterUnsafeMaxWALSize(findings []analyzer.Finding, cur walCurrent, fl walFloor,
	f walWindowFacts, logFn func(string, string, ...any)) []analyzer.Finding {
	var out []analyzer.Finding
	dropped, kept := false, false
	for _, fd := range findings {
		v, ok := maxWALSizeTarget(fd.RecommendedSQL)
		switch {
		case !ok:
			out = append(out, fd)
		case math.IsNaN(v):
			out = append(out, advisoryOnly(fd, "its max_wal_size value is unreadable"))
		case cur.known && v < cur.bytes:
			logIf(logFn, "WARN", "advisor: rejecting max_wal_size=%.0fMB: below the "+
				"current %.0fMB (pg_sage never lowers it)", v/mib, cur.bytes/mib)
			dropped = true
		case fl.known && v < fl.bytes:
			logIf(logFn, "WARN", "advisor: rejecting max_wal_size=%.0fMB: below the "+
				"measured floor %.0fMB", v/mib, fl.bytes/mib)
			dropped = true
		case !fl.known:
			out = append(out, advisoryOnly(fd, "the WAL generated over the advisor window "+
				"is unknown, so pg_sage cannot show a max_wal_size value is safe"))
		case v > walCeilingFactor*fl.bytes:
			out = append(out, advisoryOnly(fd, fmt.Sprintf("%.0fMB is above 4 x the "+
				"measured floor %.0fMB", v/mib, fl.bytes/mib)))
		default:
			out, kept = append(out, fd), true
		}
	}
	if dropped && !kept && fl.known && (!cur.known || fl.bytes > cur.bytes) {
		out = append(out, applyConfigAllowlist([]analyzer.Finding{
			walFloorFinding(cur, fl, f)})...)
	}
	return out
}

// walFloorFinding raises max_wal_size to the floor, with its arithmetic.
func walFloorFinding(cur walCurrent, fl walFloor, f walWindowFacts) analyzer.Finding {
	sql := fmt.Sprintf("ALTER SYSTEM SET max_wal_size = '%.0fMB'", fl.bytes/mib)
	arithmetic := fmt.Sprintf("%.1f x %s WAL per checkpoint_timeout (%s, measured over %s) = "+
		"%.0fMB, within %s", walFloorFactor, walSize(*f.walPerTimeout), f.checkpointTimeout,
		f.measured, fl.bytes/mib, pgconf.RangeString(pgconf.Docs["max_wal_size"]))
	detail := map[string]any{
		"max_wal_size_floor_bytes":         fl.bytes,
		"wal_per_checkpoint_timeout_bytes": *f.walPerTimeout,
		"checkpoint_timeout_seconds":       f.checkpointTimeout.Seconds(),
		"window_seconds":                   f.measured.Seconds(),
		"arithmetic":                       arithmetic,
	}
	if f.walPerHour != nil {
		detail["wal_bytes_per_hour"] = *f.walPerHour
	}
	if f.requestedRatio != nil {
		detail["requested_share"] = *f.requestedRatio
	}
	if cur.known {
		detail["current_max_wal_size_bytes"] = cur.bytes
	}
	return analyzer.Finding{Category: "wal_tuning", Severity: "info",
		ObjectType: "configuration", ObjectIdentifier: "instance",
		Title:  "wal_tuning: raise max_wal_size to the measured floor",
		Detail: detail,
		Recommendation: fmt.Sprintf("Raise max_wal_size to %.0fMB: %s. The model's value "+
			"was below the current value or this floor.", fl.bytes/mib, arithmetic),
		RecommendedSQL: sql, ActionRisk: deriveActionRisk(sql)}
}
