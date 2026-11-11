package advisor

import (
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const (
	// connFloorMargin is the headroom above the busiest moment and the
	// reserved slots: spikes and pg_sage's own pool.
	connFloorMargin = 10
	// defaultSuperuserReserved is superuser_reserved_connections' default,
	// used when the setting is missing or unreadable (never 0).
	defaultSuperuserReserved = 3
	// pgDefaultMaxConnections is what RESET max_connections returns to.
	pgDefaultMaxConnections = 100
)

// connFloor is the lowest max_connections pg_sage lets the advisor
// recommend (advisors-07): the window's peak of total backends (or the
// current count, if higher) plus the reserved slots and a margin. live is
// the floor over the current count alone, which holds even when the peak
// is unknown.
type connFloor struct {
	value, live                         int
	known                               bool
	peak, current, reserved, currentMax int
}

// connectionFloor sizes the floor from the snapshot, the server's settings
// and the window's history.
func connectionFloor(sys collector.SystemStats, settings []collector.PGSetting,
	pgMajor int, w connWindow) connFloor {
	f := connFloor{known: w.peakKnown, current: sys.TotalBackends,
		reserved: reservedSlots(settings, pgMajor), currentMax: sys.MaxConnections}
	f.live = f.current + f.reserved + connFloorMargin
	f.value = f.live
	if w.peakKnown {
		f.peak = w.peakTotal
		f.value = max(f.peak, f.current) + f.reserved + connFloorMargin
	}
	return f
}

// reservedSlots is superuser_reserved_connections plus, on PostgreSQL 16+
// (or an unknown version that reports it), reserved_connections.
func reservedSlots(settings []collector.PGSetting, pgMajor int) int {
	superuser, extra := defaultSuperuserReserved, 0
	for _, s := range settings {
		n, err := strconv.Atoi(strings.TrimSpace(s.Setting))
		if err != nil || n < 0 {
			continue
		}
		switch {
		case s.Name == "superuser_reserved_connections":
			superuser = n
		case s.Name == "reserved_connections" && (pgMajor <= 0 || pgMajor >= 16):
			extra = n
		}
	}
	return superuser + extra
}

// filterUnsafeMaxConnections is the deterministic backstop behind the
// prompt's sizing rule. A max_connections below the floor is refused: below
// the window's peak it would reject connections the workload opens every
// day. Without a peak, a value below the live floor is refused and any
// other decrease becomes advice without SQL, since its safety cannot be
// shown; increases are kept.
func filterUnsafeMaxConnections(findings []analyzer.Finding, fl connFloor,
	logFn func(string, string, ...any)) []analyzer.Finding {
	out := findings[:0]
	for _, f := range findings {
		n, ok := maxConnectionsTarget(f.RecommendedSQL)
		switch {
		case !ok:
			out = append(out, f)
		case n < fl.live || (fl.known && n < fl.value):
			logIf(logFn, "WARN", "advisor: rejecting max_connections=%d: below the safe "+
				"floor %d (peak %d, current %d total backends + %d reserved + %d margin)",
				n, max(fl.value, fl.live), fl.peak, fl.current, fl.reserved, connFloorMargin)
		case !fl.known && (fl.currentMax <= 0 || n < fl.currentMax):
			logIf(logFn, "WARN", "advisor: max_connections=%d kept as advice only: the "+
				"peak total backends over the advisor window is unknown", n)
			out = append(out, advisoryOnly(f, "the peak connection count over the "+
				"advisor window is unknown, so pg_sage does not lower max_connections"))
		default:
			out = append(out, f)
		}
	}
	return out
}

// maxConnectionsTarget is the max_connections a statement sets, in either
// SET form (= or TO); a RESET returns to PostgreSQL's default.
func maxConnectionsTarget(sql string) (int, bool) {
	if st, ok := pgconf.ParseAlterSystem(sql); ok && st.Name == "max_connections" {
		if st.Reset {
			return pgDefaultMaxConnections, true
		}
		if n, err := strconv.Atoi(strings.TrimSpace(st.Value)); err == nil {
			return n, true
		}
	}
	return parseMaxConnectionsTarget(sql)
}

func logIf(logFn func(string, string, ...any), level, msg string, args ...any) {
	if logFn != nil {
		logFn(level, msg, args...)
	}
}

// parseMaxConnectionsTarget extracts N from an
// "ALTER SYSTEM SET max_connections = N" statement.
func parseMaxConnectionsTarget(sql string) (int, bool) {
	low := strings.ToLower(sql)
	i := strings.Index(low, "max_connections")
	if i < 0 {
		return 0, false
	}
	rest := sql[i+len("max_connections"):]
	rest = strings.TrimLeft(rest, " \t=")
	rest = strings.Trim(strings.TrimSpace(rest), "'\";")
	digits := strings.Builder{}
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		digits.WriteRune(r)
	}
	if digits.Len() == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(digits.String())
	if err != nil {
		return 0, false
	}
	return n, true
}
