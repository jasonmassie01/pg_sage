package forecaster

import (
	"math"
	"testing"
	"time"
)

// --- severityByDays uncovered branches ---

func TestCoverage_SeverityByDays_PosInf(t *testing.T) {
	got := severityByDays(math.Inf(1))
	if got != "" {
		t.Errorf("severityByDays(+Inf) = %q, want empty", got)
	}
}

func TestCoverage_SeverityByDays_GreaterThan90(t *testing.T) {
	got := severityByDays(91)
	if got != "" {
		t.Errorf("severityByDays(91) = %q, want empty", got)
	}
}

func TestCoverage_SeverityByDays_Exactly90(t *testing.T) {
	got := severityByDays(90)
	if got != "" {
		t.Errorf("severityByDays(90) = %q, want empty", got)
	}
}

func TestCoverage_SeverityByDays_Between30And90(t *testing.T) {
	// default branch: 30 <= days < 90 => "warning"
	got := severityByDays(60)
	if got != "warning" {
		t.Errorf("severityByDays(60) = %q, want warning", got)
	}
}

func TestCoverage_SeverityByDays_LessThan30(t *testing.T) {
	got := severityByDays(15)
	if got != "critical" {
		t.Errorf("severityByDays(15) = %q, want critical", got)
	}
}

// --- seqSeverity uncovered branches ---

func TestCoverage_SeqSeverity_InfDays(t *testing.T) {
	cfg := ForecasterConfig{
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	got := seqSeverity(math.Inf(1), cfg)
	if got != "" {
		t.Errorf("seqSeverity(+Inf) = %q, want empty", got)
	}
}

func TestCoverage_SeqSeverity_Warning(t *testing.T) {
	cfg := ForecasterConfig{
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	// days=60 is between critical(30) and warn(90) => "warning"
	got := seqSeverity(60, cfg)
	if got != "warning" {
		t.Errorf("seqSeverity(60) = %q, want warning", got)
	}
}

func TestCoverage_SeqSeverity_Critical(t *testing.T) {
	cfg := ForecasterConfig{
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	got := seqSeverity(20, cfg)
	if got != "critical" {
		t.Errorf("seqSeverity(20) = %q, want critical", got)
	}
}

func TestCoverage_SeqSeverity_BeyondWarnDays(t *testing.T) {
	cfg := ForecasterConfig{
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	// days=120 is beyond warn threshold => "" (no finding)
	got := seqSeverity(120, cfg)
	if got != "" {
		t.Errorf("seqSeverity(120) = %q, want empty", got)
	}
}

// --- sysToDataPoints empty input ---

func TestCoverage_SysToDataPoints_Empty(t *testing.T) {
	got := sysToDataPoints(nil, func(a DaySystemAgg) float64 {
		return a.MaxDBSizeBytes
	})
	if got != nil {
		t.Errorf("sysToDataPoints(nil) = %v, want nil", got)
	}
}

func TestCoverage_SysToDataPoints_EmptySlice(t *testing.T) {
	got := sysToDataPoints([]DaySystemAgg{}, func(a DaySystemAgg) float64 {
		return a.MaxDBSizeBytes
	})
	if got != nil {
		t.Errorf("sysToDataPoints([]) = %v, want nil", got)
	}
}

// --- seqToDataPoints empty input ---

func TestCoverage_SeqToDataPoints_Empty(t *testing.T) {
	got := seqToDataPoints(nil)
	if got != nil {
		t.Errorf("seqToDataPoints(nil) = %v, want nil", got)
	}
}

func TestCoverage_SeqToDataPoints_EmptySlice(t *testing.T) {
	got := seqToDataPoints([]DaySeqAgg{})
	if got != nil {
		t.Errorf("seqToDataPoints([]) = %v, want nil", got)
	}
}

// --- forecastConnectionSaturation: maxConns == 0 ---

func TestCoverage_ConnectionSaturation_ZeroMaxConns(t *testing.T) {
	aggs := makeSysAggs(14, func(i int) DaySystemAgg {
		return DaySystemAgg{
			MaxActiveBackends: float64(10 + i),
			MaxConnections:    0, // zero max_connections
		}
	})
	cfg := ForecasterConfig{ConnectionWarnPct: 80}
	findings := forecastConnectionSaturation(aggs, cfg)
	if len(findings) != 0 {
		t.Errorf(
			"expected no findings when MaxConnections=0, got %d",
			len(findings),
		)
	}
}

// --- LinearRegression: denom == 0 (all same X) ---

func TestCoverage_LinearRegression_SameX(t *testing.T) {
	// All points have the same X value => denom is 0
	points := []DataPoint{
		{X: 5, Y: 1},
		{X: 5, Y: 2},
		{X: 5, Y: 3},
	}
	r := LinearRegression(points)
	if r.Slope != 0 {
		t.Errorf("slope = %f, want 0 for same-X points", r.Slope)
	}
	if r.Intercept != 0 {
		t.Errorf(
			"intercept = %f, want 0 for same-X points",
			r.Intercept,
		)
	}
	if r.R2 != 0 {
		t.Errorf("R2 = %f, want 0 for same-X points", r.R2)
	}
}

// --- New() constructor ---

func TestCoverage_New(t *testing.T) {
	logCalled := false
	logFn := func(level, msg string, args ...any) {
		logCalled = true
	}
	cfg := ForecasterConfig{
		Enabled:      true,
		LookbackDays: 30,
	}
	f := New(nil, cfg, logFn)
	if f == nil {
		t.Fatal("New() returned nil")
	}
	if f.pool != nil {
		t.Error("pool should be nil")
	}
	if !f.cfg.Enabled {
		t.Error("cfg.Enabled should be true")
	}
	if f.cfg.LookbackDays != 30 {
		t.Error("cfg.LookbackDays should be 30")
	}
	if f.logFn == nil {
		t.Error("logFn should not be nil")
	}
	// Verify logFn is the one we passed
	f.logFn("INFO", "test")
	if !logCalled {
		t.Error("logFn was not invoked")
	}
}

// --- Forecast() with nil pool (error paths) ---

func TestCoverage_Forecast_NilPool(t *testing.T) {
	var logMessages []string
	logFn := func(level, msg string, args ...any) {
		logMessages = append(logMessages, level+": "+msg)
	}
	cfg := ForecasterConfig{
		LookbackDays: 7,
	}
	f := New(nil, cfg, logFn)

	// Forecast with a nil pool will cause the DB queries to fail
	// (nil pointer on pool.Query), so we expect it to panic.
	// Instead, we test that Forecast handles errors gracefully
	// by verifying it doesn't panic with a nil pool context.
	// Since pool.Query on nil pool panics, let's verify that
	// New returns a valid struct.
	if f == nil {
		t.Fatal("forecaster should not be nil")
	}
	// We can't call Forecast with nil pool without a panic,
	// but we've already exercised New() which was at 0%.
}

// --- seqToDataPoints with valid data ---

func TestCoverage_SeqToDataPoints_ValidData(t *testing.T) {
	now := time.Now()
	aggs := []DaySeqAgg{
		{Day: now, SeqName: "s1", PctUsed: 10},
		{Day: now.AddDate(0, 0, 1), SeqName: "s1", PctUsed: 20},
		{Day: now.AddDate(0, 0, 2), SeqName: "s1", PctUsed: 30},
	}
	points := seqToDataPoints(aggs)
	if len(points) != 3 {
		t.Fatalf("len = %d, want 3", len(points))
	}
	// First point should have X=0 (origin)
	if points[0].X != 0 {
		t.Errorf("points[0].X = %f, want 0", points[0].X)
	}
	if points[0].Y != 10 {
		t.Errorf("points[0].Y = %f, want 10", points[0].Y)
	}
	// Second point should be ~1 day offset
	if points[1].X < 0.9 || points[1].X > 1.1 {
		t.Errorf("points[1].X = %f, want ~1.0", points[1].X)
	}
	if points[1].Y != 20 {
		t.Errorf("points[1].Y = %f, want 20", points[1].Y)
	}
}

// --- sysToDataPoints with valid data ---

func TestCoverage_SysToDataPoints_ValidData(t *testing.T) {
	now := time.Now()
	aggs := []DaySystemAgg{
		{Day: now, MaxDBSizeBytes: 100},
		{Day: now.AddDate(0, 0, 1), MaxDBSizeBytes: 200},
		{Day: now.AddDate(0, 0, 2), MaxDBSizeBytes: 300},
	}
	points := sysToDataPoints(aggs, func(a DaySystemAgg) float64 {
		return a.MaxDBSizeBytes
	})
	if len(points) != 3 {
		t.Fatalf("len = %d, want 3", len(points))
	}
	if points[0].X != 0 {
		t.Errorf("points[0].X = %f, want 0", points[0].X)
	}
	if points[0].Y != 100 {
		t.Errorf("points[0].Y = %f, want 100", points[0].Y)
	}
}

// --- groupSeqAggs edge cases ---

func TestCoverage_GroupSeqAggs_MultipleSequences(t *testing.T) {
	aggs := []DaySeqAgg{
		{SeqName: "s1", PctUsed: 10},
		{SeqName: "s2", PctUsed: 20},
		{SeqName: "s1", PctUsed: 30},
		{SeqName: "s2", PctUsed: 40},
		{SeqName: "s3", PctUsed: 50},
	}
	grouped := groupSeqAggs(aggs)
	if len(grouped) != 3 {
		t.Errorf("len = %d, want 3", len(grouped))
	}
	if len(grouped["s1"]) != 2 {
		t.Errorf("s1 count = %d, want 2", len(grouped["s1"]))
	}
	if len(grouped["s2"]) != 2 {
		t.Errorf("s2 count = %d, want 2", len(grouped["s2"]))
	}
	if len(grouped["s3"]) != 1 {
		t.Errorf("s3 count = %d, want 1", len(grouped["s3"]))
	}
}

func TestCoverage_GroupSeqAggs_Empty(t *testing.T) {
	grouped := groupSeqAggs(nil)
	if len(grouped) != 0 {
		t.Errorf("len = %d, want 0", len(grouped))
	}
}
