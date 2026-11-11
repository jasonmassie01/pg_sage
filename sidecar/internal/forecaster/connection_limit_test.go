package forecaster

import (
	"math"
	"testing"
)

// forecast-08: connection saturation counts client backends against the
// slots a client can take, max_connections less the reserved ones. Rows
// written before the collector stored both fall back to total backends
// against max_connections, flagged limit_basis legacy.

// No concurrent access tests here: the rule is a pure function;
// connection_limit_db_test.go runs two Forecast calls at once.

// clientDays is n days of client peaks rising 3 a day from 40, with 60
// more background and pg_sage backends in the total, max_connections 200
// and 3 reserved slots; the days in legacy carry neither new count.
func clientDays(n int, legacy ...int) []DaySystemAgg {
	old := map[int]bool{}
	for _, i := range legacy {
		old[i] = true
	}
	return makeSysAggs(n, func(i int) DaySystemAgg {
		a := DaySystemAgg{MaxClientBackends: float64(40 + 3*i),
			MaxTotalBackends: float64(100 + 3*i), MaxActiveBackends: 5,
			MaxReservedConnections: 3, MaxConnections: 200, HasClientCounts: true}
		if old[i] {
			a.MaxClientBackends, a.MaxReservedConnections, a.HasClientCounts = 0, 0, false
		}
		return a
	})
}

func TestConnectionLimit_ClientBackendsAgainstUsableSlots(t *testing.T) {
	aggs := clientDays(14)
	f := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(f) != 1 {
		t.Fatalf("findings = %d, want 1", len(f))
	}
	d := f[0].Detail
	if d["limit_basis"] != limitBasisClient {
		t.Fatalf("limit_basis = %v, want %s", d["limit_basis"], limitBasisClient)
	}
	// 79 client backends now; (200 - 3) x 80% = 157.6; 3 a day.
	if d["current_in_use"] != 79.0 || d["usable_connections"] != 197.0 ||
		d["reserved_connections"] != 3.0 {
		t.Errorf("detail = %v, want 79 in use of 197 usable (3 reserved)", d)
	}
	if days := d["days_to_warn_pct"].(float64); math.Abs(days-(157.6-79)/3) > 1e-9 {
		t.Errorf("days_to_warn_pct = %v, want %v", days, (157.6-79)/3)
	}
	if f[0].Severity != "critical" {
		t.Errorf("severity = %q, want critical (26 days)", f[0].Severity)
	}
}

// Legacy rows: fewer than 7 days carry client_backends and
// reserved_connections, so the rule keeps total backends against
// max_connections and says so.
func TestConnectionLimit_LegacyRowsFallBackToTotalBackends(t *testing.T) {
	aggs := clientDays(14, 0, 1, 2, 3, 4, 5, 6, 7) // 6 new days
	f := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(f) != 1 {
		t.Fatalf("findings = %d, want 1", len(f))
	}
	d := f[0].Detail
	if d["limit_basis"] != limitBasisLegacy {
		t.Fatalf("limit_basis = %v, want %s", d["limit_basis"], limitBasisLegacy)
	}
	// 139 total backends now; 200 x 80% = 160.
	if d["current_in_use"] != 139.0 || d["usable_connections"] != 200.0 {
		t.Errorf("detail = %v, want 139 total backends of 200", d)
	}
	if d["covered_days"] != 14 {
		t.Errorf("covered_days = %v, want all 14 days on the legacy basis", d["covered_days"])
	}
}

// Boundary: seven new days are enough, and only they are fitted.
func TestConnectionLimit_SevenNewDaysSwitchTheBasis(t *testing.T) {
	aggs := clientDays(14, 0, 1, 2, 3, 4, 5, 6)
	f := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(f) != 1 || f[0].Detail["limit_basis"] != limitBasisClient {
		t.Fatalf("findings = %+v, want one on the client basis", f)
	}
	if f[0].Detail["covered_days"] != 7 {
		t.Errorf("covered_days = %v, want the 7 new days", f[0].Detail["covered_days"])
	}
}

// Invalid input: reserved slots at or above max_connections leave no
// usable slot to project against.
func TestConnectionLimit_NoUsableSlotsNoFinding(t *testing.T) {
	aggs := clientDays(14)
	for i := range aggs {
		aggs[i].MaxReservedConnections = 200
	}
	cfg := ForecasterConfig{ConnectionWarnPct: 80}
	if f := forecastConnectionSaturation(aggs, cfg); len(f) != 0 {
		t.Fatalf("findings = %+v, want none without a usable slot", f)
	}
}

// The client basis must not report background workers as load: the
// totals rise 3 a day too, but a flat client count gives no finding.
func TestConnectionLimit_FlatClientsRisingTotalsNoFinding(t *testing.T) {
	aggs := clientDays(14)
	for i := range aggs {
		aggs[i].MaxClientBackends = 40
		aggs[i].MaxTotalBackends = float64(100 + 5*i)
	}
	cfg := ForecasterConfig{ConnectionWarnPct: 80}
	if f := forecastConnectionSaturation(aggs, cfg); len(f) != 0 {
		t.Fatalf("findings = %+v, want none: client backends are flat", f)
	}
}
