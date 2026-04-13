package forecaster

import (
	"math"
	"testing"
	"time"
)

// floatNear returns true if |a - b| < epsilon.
func floatNear(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

// makeLinearPoints generates n data points along y = slope*x + base,
// spaced one day apart starting at t0.
func makeLinearPoints(
	t0 time.Time, n int, base, slopePerDay float64,
) []SizeDataPoint {
	pts := make([]SizeDataPoint, n)
	for i := 0; i < n; i++ {
		t := t0.Add(time.Duration(i*24) * time.Hour)
		pts[i] = SizeDataPoint{
			CollectedAt: t,
			SizeBytes:   int64(base + slopePerDay*float64(i)),
		}
	}
	return pts
}

// ---------------------------------------------------------------
// linearRegression tests
// ---------------------------------------------------------------

func TestLinearRegression_PerfectLinear(t *testing.T) {
	// y = 100 bytes/day. Points spaced 1 day apart.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := makeLinearPoints(t0, 10, 1000, 100)

	slope, intercept, r2 := linearRegression(pts)

	// slope is in bytes/sec; 100 bytes/day = 100/86400 bytes/sec
	expectedSlope := 100.0 / secondsPerDay
	if !floatNear(slope, expectedSlope, 1e-9) {
		t.Errorf("slope: got %v, want ~%v", slope, expectedSlope)
	}

	// intercept = base at t0 in Unix seconds; verify predicted
	// value at t0 matches 1000.
	predicted := slope*float64(t0.Unix()) + intercept
	if !floatNear(predicted, 1000, 0.5) {
		t.Errorf(
			"predicted at t0: got %v, want ~1000", predicted,
		)
	}

	if !floatNear(r2, 1.0, 1e-6) {
		t.Errorf("R2: got %v, want ~1.0", r2)
	}
}

func TestLinearRegression_Constant(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := make([]SizeDataPoint, 5)
	for i := range pts {
		pts[i] = SizeDataPoint{
			CollectedAt: t0.Add(
				time.Duration(i*24) * time.Hour,
			),
			SizeBytes: 5000,
		}
	}

	slope, _, r2 := linearRegression(pts)

	if !floatNear(slope, 0, 1e-12) {
		t.Errorf("slope: got %v, want 0", slope)
	}
	// ssTot == 0 when all y are equal, so R2 path returns 0.
	if r2 != 0 {
		t.Errorf("R2: got %v, want 0", r2)
	}
}

func TestLinearRegression_SinglePoint(t *testing.T) {
	pts := []SizeDataPoint{
		{
			CollectedAt: time.Now(),
			SizeBytes:   42,
		},
	}
	slope, intercept, r2 := linearRegression(pts)
	if slope != 0 || intercept != 0 || r2 != 0 {
		t.Errorf(
			"single point: got (%v, %v, %v), want (0, 0, 0)",
			slope, intercept, r2,
		)
	}
}

func TestLinearRegression_TwoPoints(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	pts := []SizeDataPoint{
		{CollectedAt: t0, SizeBytes: 1000},
		{
			CollectedAt: t0.Add(24 * time.Hour),
			SizeBytes:   2000,
		},
	}

	slope, _, r2 := linearRegression(pts)

	// 1000 bytes/day in bytes/sec.
	expectedSlope := 1000.0 / secondsPerDay
	if !floatNear(slope, expectedSlope, 1e-9) {
		t.Errorf("slope: got %v, want ~%v", slope, expectedSlope)
	}
	// Two points on a line -> perfect fit.
	if !floatNear(r2, 1.0, 1e-6) {
		t.Errorf("R2: got %v, want 1.0", r2)
	}
}

func TestLinearRegression_Noisy(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Underlying trend: 500 bytes/day, but with noise.
	raw := []int64{
		1000, 1600, 1900, 2700, 3100,
		3400, 4200, 4500, 5300, 5800,
	}
	pts := make([]SizeDataPoint, len(raw))
	for i, v := range raw {
		pts[i] = SizeDataPoint{
			CollectedAt: t0.Add(
				time.Duration(i*24) * time.Hour,
			),
			SizeBytes: v,
		}
	}

	slope, _, r2 := linearRegression(pts)

	// Slope should be roughly 500/86400 bytes/sec.
	expectedSlope := 500.0 / secondsPerDay
	if !floatNear(slope, expectedSlope, expectedSlope*0.3) {
		t.Errorf(
			"slope: got %v, want ~%v (±30%%)",
			slope, expectedSlope,
		)
	}
	if r2 <= 0 || r2 >= 1.0 {
		t.Errorf(
			"R2: got %v, want 0 < R2 < 1", r2,
		)
	}
}

func TestLinearRegression_NegativeSlope(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Shrinking: 10000 - 200/day
	pts := makeLinearPoints(t0, 7, 10000, -200)

	slope, _, r2 := linearRegression(pts)

	if slope >= 0 {
		t.Errorf("slope: got %v, want negative", slope)
	}
	expectedSlope := -200.0 / secondsPerDay
	if !floatNear(slope, expectedSlope, 1e-9) {
		t.Errorf(
			"slope: got %v, want ~%v", slope, expectedSlope,
		)
	}
	if !floatNear(r2, 1.0, 1e-6) {
		t.Errorf("R2: got %v, want ~1.0", r2)
	}
}

// ---------------------------------------------------------------
// forecastGrowth tests
// ---------------------------------------------------------------

func TestForecastGrowth_TooFewPoints(t *testing.T) {
	t0 := time.Now()
	pts := []SizeDataPoint{
		{CollectedAt: t0, SizeBytes: 100},
	}
	fc := forecastGrowth(pts, 10000, 3, 0.5)
	if fc != nil {
		t.Error("expected nil for too few points")
	}
}

func TestForecastGrowth_GrowingToCapacity(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 1000 bytes/day growth, starting at 5000.
	pts := makeLinearPoints(t0, 10, 5000, 1000)
	// Capacity at 20000. Latest point is day 9 => 14000 bytes.
	// Remaining = 6000, rate = 1000/day => 6 days.
	capacity := int64(20000)
	fc := forecastGrowth(pts, capacity, 3, 0.5)

	if fc == nil {
		t.Fatal("expected non-nil forecast")
	}
	if fc.DaysUntilFull <= 0 {
		t.Errorf(
			"DaysUntilFull: got %d, want positive",
			fc.DaysUntilFull,
		)
	}

	// Latest is day 9: 5000 + 9*1000 = 14000
	if fc.CurrentBytes != 14000 {
		t.Errorf(
			"CurrentBytes: got %d, want 14000",
			fc.CurrentBytes,
		)
	}

	// remaining = 6000, rate = 1000/day => ceil(6) = 6 days
	if fc.DaysUntilFull != 6 {
		t.Errorf(
			"DaysUntilFull: got %d, want 6",
			fc.DaysUntilFull,
		)
	}

	if fc.ThresholdBytes != capacity {
		t.Errorf(
			"ThresholdBytes: got %d, want %d",
			fc.ThresholdBytes, capacity,
		)
	}

	if fc.DataPoints != 10 {
		t.Errorf("DataPoints: got %d, want 10", fc.DataPoints)
	}

	// ProjectedDate should be ~6 days after last point.
	lastPt := t0.Add(9 * 24 * time.Hour)
	daysDiff := fc.ProjectedDate.Sub(lastPt).Hours() / 24
	if !floatNear(daysDiff, 6.0, 0.1) {
		t.Errorf(
			"ProjectedDate offset: got %.2f days, want ~6",
			daysDiff,
		)
	}
}

func TestForecastGrowth_AlreadyFull(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Growing data, but current already exceeds capacity.
	pts := makeLinearPoints(t0, 5, 10000, 500)
	// Latest = 10000 + 4*500 = 12000; capacity = 10000.
	fc := forecastGrowth(pts, 10000, 3, 0.5)

	if fc == nil {
		t.Fatal("expected non-nil forecast")
	}
	if fc.DaysUntilFull != 0 {
		t.Errorf(
			"DaysUntilFull: got %d, want 0",
			fc.DaysUntilFull,
		)
	}
	// ProjectedDate should equal latest point's time.
	lastPt := t0.Add(4 * 24 * time.Hour)
	if !fc.ProjectedDate.Equal(lastPt) {
		t.Errorf(
			"ProjectedDate: got %v, want %v",
			fc.ProjectedDate, lastPt,
		)
	}
}

func TestForecastGrowth_ShrinkingData(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := makeLinearPoints(t0, 5, 10000, -500)

	fc := forecastGrowth(pts, 20000, 3, 0.5)

	if fc == nil {
		t.Fatal("expected non-nil forecast")
	}
	if fc.DaysUntilFull != -1 {
		t.Errorf(
			"DaysUntilFull: got %d, want -1",
			fc.DaysUntilFull,
		)
	}
}

func TestForecastGrowth_ZeroCapacity(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := makeLinearPoints(t0, 5, 5000, 100)

	fc := forecastGrowth(pts, 0, 3, 0.5)

	if fc == nil {
		t.Fatal("expected non-nil forecast")
	}
	if fc.DaysUntilFull != -1 {
		t.Errorf(
			"DaysUntilFull: got %d, want -1",
			fc.DaysUntilFull,
		)
	}
}

func TestForecastGrowth_GrowthRateCalculation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 2000 bytes/day growth.
	pts := makeLinearPoints(t0, 7, 1000, 2000)

	fc := forecastGrowth(pts, 100000, 3, 0.5)
	if fc == nil {
		t.Fatal("expected non-nil forecast")
	}

	// GrowthRateBytes should be slope * 86400.
	// For perfect linear data, slope = 2000/86400 bytes/sec.
	// slope * 86400 = 2000 bytes/day.
	expected := int64(2000)
	if fc.GrowthRateBytes != expected {
		t.Errorf(
			"GrowthRateBytes: got %d, want %d",
			fc.GrowthRateBytes, expected,
		)
	}
}

// ---------------------------------------------------------------
// growthSeverity tests (table-driven)
// ---------------------------------------------------------------

func TestGrowthSeverity(t *testing.T) {
	tests := []struct {
		name    string
		days    int
		r2      float64
		minR2   float64
		wantSev string
		wantRel bool
	}{
		{
			name:    "critical: days=2, high R2",
			days:    2,
			r2:      0.9,
			minR2:   0.5,
			wantSev: "critical",
			wantRel: true,
		},
		{
			name:    "warning: days=5, medium R2",
			days:    5,
			r2:      0.6,
			minR2:   0.5,
			wantSev: "warning",
			wantRel: true,
		},
		{
			name:    "info: days=20, good R2",
			days:    20,
			r2:      0.7,
			minR2:   0.5,
			wantSev: "info",
			wantRel: true,
		},
		{
			name:    "unreliable: days=15, low R2",
			days:    15,
			r2:      0.3,
			minR2:   0.5,
			wantSev: "info",
			wantRel: false,
		},
		{
			name:    "no finding: days=40",
			days:    40,
			r2:      0.9,
			minR2:   0.5,
			wantSev: "",
			wantRel: false,
		},
		{
			name:    "boundary: days=3, low R2",
			days:    3,
			r2:      0.3,
			minR2:   0.5,
			wantSev: "info",
			wantRel: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sev, rel := growthSeverity(
				tc.days, tc.r2, tc.minR2,
			)
			if sev != tc.wantSev {
				t.Errorf(
					"severity: got %q, want %q",
					sev, tc.wantSev,
				)
			}
			if rel != tc.wantRel {
				t.Errorf(
					"reliable: got %v, want %v",
					rel, tc.wantRel,
				)
			}
		})
	}
}

// ---------------------------------------------------------------
// GrowthFindings tests
// ---------------------------------------------------------------

func makeForecast(
	metric, obj string,
	current, threshold int64,
	days int,
	r2 float64,
	dbName string,
) LinearForecast {
	return LinearForecast{
		Metric:          metric,
		ObjectName:      obj,
		CurrentBytes:    current,
		GrowthRateBytes: 1000,
		ThresholdBytes:  threshold,
		DaysUntilFull:   days,
		ProjectedDate: time.Now().Add(
			time.Duration(days*24) * time.Hour,
		),
		R2:           r2,
		DataPoints:   10,
		DatabaseName: dbName,
	}
}

func TestGrowthFindings_Empty(t *testing.T) {
	findings := GrowthFindings(nil, 0.5)
	if len(findings) != 0 {
		t.Errorf(
			"expected 0 findings, got %d", len(findings),
		)
	}
}

func TestGrowthFindings_CriticalForecast(t *testing.T) {
	fc := makeForecast(
		"table_size", "orders", 9000, 10000, 2, 0.95, "mydb",
	)
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 1 {
		t.Fatalf(
			"expected 1 finding, got %d", len(findings),
		)
	}
	if findings[0].Severity != "critical" {
		t.Errorf(
			"severity: got %q, want 'critical'",
			findings[0].Severity,
		)
	}
}

func TestGrowthFindings_WarningForecast(t *testing.T) {
	fc := makeForecast(
		"table_size", "users", 5000, 10000, 6, 0.7, "mydb",
	)
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 1 {
		t.Fatalf(
			"expected 1 finding, got %d", len(findings),
		)
	}
	if findings[0].Severity != "warning" {
		t.Errorf(
			"severity: got %q, want 'warning'",
			findings[0].Severity,
		)
	}
}

func TestGrowthFindings_InfoForecast(t *testing.T) {
	fc := makeForecast(
		"index_size", "idx_pk", 3000, 10000, 20, 0.65, "mydb",
	)
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 1 {
		t.Fatalf(
			"expected 1 finding, got %d", len(findings),
		)
	}
	if findings[0].Severity != "info" {
		t.Errorf(
			"severity: got %q, want 'info'",
			findings[0].Severity,
		)
	}
}

func TestGrowthFindings_UnreliableForecast(t *testing.T) {
	// R2=0.3, below minR2=0.5 -> info, unreliable.
	fc := makeForecast(
		"table_size", "logs", 8000, 10000, 10, 0.3, "mydb",
	)
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 1 {
		t.Fatalf(
			"expected 1 finding, got %d", len(findings),
		)
	}

	f := findings[0]
	if f.Severity != "info" {
		t.Errorf(
			"severity: got %q, want 'info'", f.Severity,
		)
	}
	wantSubstr := "(forecast unreliable)"
	if !containsStr(f.Title, wantSubstr) {
		t.Errorf(
			"title %q missing %q", f.Title, wantSubstr,
		)
	}
	// Recommendation should mention R2 and "Insufficient".
	if !containsStr(f.Recommendation, "Insufficient") {
		t.Errorf(
			"recommendation %q missing 'Insufficient'",
			f.Recommendation,
		)
	}
}

func TestGrowthFindings_NoForecast_Shrinking(t *testing.T) {
	fc := makeForecast(
		"table_size", "shrink", 5000, 10000, -1, 0.9, "mydb",
	)
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 0 {
		t.Errorf(
			"expected 0 findings for shrinking, got %d",
			len(findings),
		)
	}
}

func TestGrowthFindings_NoForecast_Distant(t *testing.T) {
	fc := makeForecast(
		"table_size", "distant", 1000, 100000, 60, 0.95, "mydb",
	)
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 0 {
		t.Errorf(
			"expected 0 findings for distant forecast, got %d",
			len(findings),
		)
	}
}

func TestGrowthFindings_FindingFields(t *testing.T) {
	fc := LinearForecast{
		Metric:          "table_size",
		ObjectName:      "orders",
		CurrentBytes:    8000,
		GrowthRateBytes: 500,
		ThresholdBytes:  10000,
		DaysUntilFull:   4,
		ProjectedDate: time.Date(
			2026, 4, 16, 0, 0, 0, 0, time.UTC,
		),
		R2:           0.88,
		DataPoints:   14,
		DatabaseName: "prod",
	}
	findings := GrowthFindings([]LinearForecast{fc}, 0.5)
	if len(findings) != 1 {
		t.Fatalf(
			"expected 1 finding, got %d", len(findings),
		)
	}

	f := findings[0]

	// Category
	if f.Category != "storage_forecast" {
		t.Errorf(
			"Category: got %q, want 'storage_forecast'",
			f.Category,
		)
	}

	// Severity: days=4, R2=0.88 => days<=7 && r2>=0.5 => warning
	if f.Severity != "warning" {
		t.Errorf(
			"Severity: got %q, want 'warning'", f.Severity,
		)
	}

	// ObjectType should be the metric.
	if f.ObjectType != "table_size" {
		t.Errorf(
			"ObjectType: got %q, want 'table_size'",
			f.ObjectType,
		)
	}

	// ObjectIdentifier should be the object name.
	if f.ObjectIdentifier != "orders" {
		t.Errorf(
			"ObjectIdentifier: got %q, want 'orders'",
			f.ObjectIdentifier,
		)
	}

	// Title should contain metric, object, and days.
	if !containsStr(f.Title, "table_size") ||
		!containsStr(f.Title, "orders") ||
		!containsStr(f.Title, "4 days") {
		t.Errorf("Title missing expected content: %q", f.Title)
	}

	// DatabaseName
	if f.DatabaseName != "prod" {
		t.Errorf(
			"DatabaseName: got %q, want 'prod'",
			f.DatabaseName,
		)
	}

	// Recommendation should mention the object, days, and rate.
	if !containsStr(f.Recommendation, "orders") ||
		!containsStr(f.Recommendation, "4 days") ||
		!containsStr(f.Recommendation, "500 bytes/day") {
		t.Errorf(
			"Recommendation missing content: %q",
			f.Recommendation,
		)
	}

	// Detail map keys.
	requiredKeys := []string{
		"forecast_type",
		"current_bytes",
		"growth_rate_bytes_day",
		"threshold_bytes",
		"days_until_full",
		"projected_date",
		"r_squared",
		"data_points",
	}
	for _, key := range requiredKeys {
		if _, ok := f.Detail[key]; !ok {
			t.Errorf("Detail missing key %q", key)
		}
	}

	// Verify specific detail values.
	if v, ok := f.Detail["current_bytes"].(int64); !ok || v != 8000 {
		t.Errorf(
			"Detail[current_bytes]: got %v, want 8000",
			f.Detail["current_bytes"],
		)
	}
	if v, ok := f.Detail["days_until_full"].(int); !ok || v != 4 {
		t.Errorf(
			"Detail[days_until_full]: got %v, want 4",
			f.Detail["days_until_full"],
		)
	}
	if v, ok := f.Detail["data_points"].(int); !ok || v != 14 {
		t.Errorf(
			"Detail[data_points]: got %v, want 14",
			f.Detail["data_points"],
		)
	}
}

func TestGrowthFindings_MultipleMixed(t *testing.T) {
	forecasts := []LinearForecast{
		// Critical: days=2, high R2.
		makeForecast(
			"table_size", "critical_tbl",
			9500, 10000, 2, 0.95, "db1",
		),
		// Warning: days=5, good R2.
		makeForecast(
			"index_size", "warn_idx",
			7000, 10000, 5, 0.7, "db1",
		),
		// Shrinking: no finding.
		makeForecast(
			"table_size", "shrink_tbl",
			3000, 10000, -1, 0.9, "db2",
		),
		// Distant: no finding.
		makeForecast(
			"table_size", "distant_tbl",
			1000, 100000, 90, 0.99, "db2",
		),
		// Unreliable: still produces a finding.
		makeForecast(
			"table_size", "unreliable_tbl",
			8000, 10000, 15, 0.2, "db3",
		),
	}

	findings := GrowthFindings(forecasts, 0.5)

	// Should get 3 findings: critical, warning, unreliable info.
	if len(findings) != 3 {
		t.Fatalf(
			"expected 3 findings, got %d", len(findings),
		)
	}

	// Verify severities in order (same order as input).
	wantSev := []string{"critical", "warning", "info"}
	for i, want := range wantSev {
		if findings[i].Severity != want {
			t.Errorf(
				"findings[%d] severity: got %q, want %q",
				i, findings[i].Severity, want,
			)
		}
	}

	// Verify the unreliable one is tagged.
	last := findings[2]
	if !containsStr(last.Title, "(forecast unreliable)") {
		t.Errorf(
			"unreliable finding title missing tag: %q",
			last.Title,
		)
	}
}

// containsStr checks if s contains substr.
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) &&
		// Use a simple scan to avoid importing strings.
		findSubstr(s, substr)
}

func findSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
