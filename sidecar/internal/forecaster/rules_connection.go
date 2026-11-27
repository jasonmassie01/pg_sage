package forecaster

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Connection saturation bases (forecast-08). client_backends counts the
// cluster's client sessions against the slots a client can take,
// max_connections less superuser_reserved_connections and (PG 16+)
// reserved_connections. Rows written before the collector stored both
// fall back to every backend against max_connections until minDataPoints
// days carry them.
const (
	limitBasisClient = "client_backends"
	limitBasisLegacy = "legacy"
)

// connectionsInUse counts every backend holding a connection slot. Idle
// and idle-in-transaction sessions consume slots too, so saturation is
// measured on total backends, not active ones (G2-B18). It is the legacy
// basis.
func connectionsInUse(a DaySystemAgg) float64 {
	return math.Max(a.MaxTotalBackends, a.MaxActiveBackends)
}

// connectionSeries is the days a connection projection fits, what it
// counts and against which limit.
type connectionSeries struct {
	days   []DaySystemAgg
	basis  string
	inUse  func(DaySystemAgg) float64
	usable func(DaySystemAgg) (usable, reserved float64)
}

// connectionSeriesOf picks the client basis when minDataPoints known days
// carry client_backends and reserved_connections (only those days are
// fitted), else the legacy basis over every known day.
func connectionSeriesOf(aggs []DaySystemAgg) connectionSeries {
	known := knownConnectionDays(aggs)
	client := knownDays(known, func(a DaySystemAgg) bool { return !a.HasClientCounts })
	if len(client) >= minDataPoints {
		return connectionSeries{days: client, basis: limitBasisClient,
			inUse: func(a DaySystemAgg) float64 { return a.MaxClientBackends },
			usable: func(a DaySystemAgg) (float64, float64) {
				return a.MaxConnections - a.MaxReservedConnections, a.MaxReservedConnections
			}}
	}
	return connectionSeries{days: known, basis: limitBasisLegacy, inUse: connectionsInUse,
		usable: func(a DaySystemAgg) (float64, float64) { return a.MaxConnections, 0 }}
}

// forecastConnectionSaturation projects when connections in use
// will reach connection_warn_pct of the usable slots. Days without
// backend counts or max_connections are skipped (forecast-07); the
// trend must pass trendGate (forecast-08).
func forecastConnectionSaturation(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	s := connectionSeriesOf(aggs)
	if len(s.days) < minDataPoints {
		return nil
	}
	reg := LinearRegression(sysToDataPoints(s.days, s.inUse))
	gate := trendGate(s.days, reg, cfg)
	latest := s.days[len(s.days)-1]
	usable, reserved := s.usable(latest)
	if !gate.ok || !(usable > 0) {
		return nil
	}
	current := s.inUse(latest)
	days := DaysUntilThreshold(current, reg.Slope, usable*cfg.ConnectionWarnPct/100)
	severity := severityByDays(days)
	date, dated := projectedDate(latest.Day, days)
	if severity == "" || !dated {
		return nil
	}
	return []analyzer.Finding{connectionFinding(cfg, severity, date, days, map[string]any{
		"forecast_type":        "connection_saturation",
		"current_in_use":       current,
		"max_connections":      latest.MaxConnections,
		"usable_connections":   usable,
		"reserved_connections": reserved,
		"limit_basis":          s.basis,
		"slope_per_day":        reg.Slope,
		"days_to_warn_pct":     days,
		"projected_date":       date,
		"r_squared":            reg.R2,
		"span_days":            gate.spanDays,
		"covered_days":         gate.coveredDays,
	})}
}

func connectionFinding(cfg ForecasterConfig, severity, date string, days float64,
	detail map[string]any) analyzer.Finding {
	limit := "max_connections less reserved slots"
	if detail["limit_basis"] == limitBasisLegacy {
		limit = "max_connections"
	}
	return analyzer.Finding{
		Category:   "forecast_connection_saturation",
		Severity:   severity,
		ObjectType: "database",
		Title: fmt.Sprintf("Peak connections projected to reach %.0f%% of %s by %s "+
			"(in %.0f days)", cfg.ConnectionWarnPct, limit, date, days),
		Detail: detail,
		Recommendation: fmt.Sprintf(
			"Connections in use (active + idle) are trending toward %.0f%% "+
				"of %s. Consider connection pooling or increasing max_connections.",
			cfg.ConnectionWarnPct, limit),
	}
}
