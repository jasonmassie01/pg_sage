package cases_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Urgency is read only from the fields the producers write (cases-07).
// These tests build each finding with its real producer and project it
// as the API does: the detail round-trips through JSON, as it does
// through sage.findings.detail.
// No concurrent access tests: UrgencyForFinding is a pure function of one
// finding, computed per request.

var urgencyAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func urgencyOptions() runway.Options {
	return runway.Options{Database: "orders", MinSamples: 10, MinSpan: 30 * time.Minute,
		WraparoundHorizon: 336 * time.Hour, WraparoundCritical: 72 * time.Hour,
		DiskHorizon: 96 * time.Hour, DiskCritical: 24 * time.Hour,
		SequenceHorizon: 30 * 24 * time.Hour, SequenceCritical: 7 * 24 * time.Hour,
		Lookback: 6 * time.Hour}
}

func runwayTrend(kind, subject string, n int64, span time.Duration, last, limit float64,
	to time.Duration, r2 float64) probes.RunwayTrend {
	return probes.RunwayTrend{Kind: kind, Subject: subject, Samples: n,
		FirstAt: urgencyAt.Add(-span), LastAt: urgencyAt, LastValue: last, Limit: limit,
		RatePerS: (limit - last) / to.Seconds(), R2: r2}
}

// asSource is the finding as the API reads it back from sage.findings.
func asSource(t *testing.T, f analyzer.Finding) cases.SourceFinding {
	t.Helper()
	raw, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatalf("marshal detail %v: %v", f.Detail, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return cases.SourceFinding{ID: "1", DatabaseName: "orders", Category: f.Category,
		Severity: cases.Severity(f.Severity), ObjectType: f.ObjectType,
		ObjectIdentifier: f.ObjectIdentifier, Title: f.Title, Detail: detail,
		ObservedAt: urgencyAt}
}

func onlyRunway(t *testing.T, in runway.EvalInput) analyzer.Finding {
	t.Helper()
	rs, _ := runway.Evaluate(in, urgencyOptions())
	if len(rs) != 1 {
		t.Fatalf("runway.Evaluate = %+v, want one runway", rs)
	}
	return rs[0].Finding()
}

func wantAssessed(t *testing.T, u cases.Urgency, secs float64, basis string) {
	t.Helper()
	if !u.Assessed || u.TimeToImpactSeconds == nil {
		t.Fatalf("urgency = %+v, want assessed", u)
	}
	if math.Abs(*u.TimeToImpactSeconds-secs) > 1 {
		t.Fatalf("time to impact = %v s, want %v s", *u.TimeToImpactSeconds, secs)
	}
	if u.Basis != basis {
		t.Fatalf("basis = %q, want %q", u.Basis, basis)
	}
}

func wantUnassessed(t *testing.T, u cases.Urgency) {
	t.Helper()
	if u.Assessed || u.TimeToImpactSeconds != nil {
		t.Fatalf("urgency = %+v, want unassessed with no number", u)
	}
}

func TestUrgency_DiskRunwayFromTheRunwayProducer(t *testing.T) {
	tr := runwayTrend(probes.RunwayDiskUsed, probes.SubjectCluster, 48, 6*time.Hour,
		9e10, 1e11, 72*time.Hour, 0.97)
	f := onlyRunway(t, runway.EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true})
	c := cases.ProjectFinding(asSource(t, f), nil)

	wantAssessed(t, c.Urgency, 259200,
		"seconds_to_limit 259200 from 48 samples over 6 h, R² 0.97")
	if c.WhyNow != "About 3 days until the disk is full "+
		"(seconds_to_limit 259200 from 48 samples over 6 h, R² 0.97)" {
		t.Fatalf("why now = %q", c.WhyNow)
	}
	if c.Urgency.Impact != "until the disk is full" {
		t.Fatalf("impact = %q", c.Urgency.Impact)
	}
	if got := c.Urgency.Inputs["rate_per_s"]; got == nil {
		t.Fatalf("inputs lack rate_per_s: %v", c.Urgency.Inputs)
	}
}

func TestUrgency_SlotAndSequenceRunways(t *testing.T) {
	slot := runwayTrend(probes.RunwayWALSlot, "cdc", 12, time.Hour, 5e9, 10<<30,
		16*time.Hour, 0.95)
	f := onlyRunway(t, runway.EvalInput{Trends: []probes.RunwayTrend{slot}, TrendsOK: true})
	u := cases.UrgencyForFinding(asSource(t, f))
	wantAssessed(t, u, 57600, "seconds_to_limit 57600 from 12 samples over 1 h, R² 0.95")
	if u.Impact != "until the slot reaches its WAL maximum" {
		t.Fatalf("slot impact = %q", u.Impact)
	}

	seq := runwayTrend(probes.RunwaySequence, "public.a_seq", 12, time.Hour, 2e9,
		2147483647, 10*24*time.Hour, 1)
	f = onlyRunway(t, runway.EvalInput{Trends: []probes.RunwayTrend{seq}, TrendsOK: true,
		SequencesOK: true, Sequences: []probes.SequenceRunway{{Sequence: "public.a_seq"}}})
	u = cases.UrgencyForFinding(asSource(t, f))
	wantAssessed(t, u, 864000, "seconds_to_limit 864000 from 12 samples over 1 h, R² 1.00")
	if u.Impact != "until the sequence reaches its limit" {
		t.Fatalf("sequence impact = %q", u.Impact)
	}
}

func storage(detail map[string]any) cases.SourceFinding {
	return cases.SourceFinding{Category: "forecast_disk_growth", Severity: cases.SeverityWarning,
		Detail: detail}
}

func TestUrgency_ZeroNegativeAndMissingAreUnassessed(t *testing.T) {
	for name, detail := range map[string]map[string]any{
		"zero":     {"days_until_full": 0, "r_squared": 0.9, "covered_days": 30},
		"negative": {"days_until_full": -1, "r_squared": 0.9, "covered_days": 30},
		"missing":  {"r_squared": 0.9, "covered_days": 30},
		"nil map":  nil,
	} {
		c := cases.ProjectFinding(storage(detail), nil)
		wantUnassessed(t, c.Urgency)
		if strings.Contains(c.WhyNow, "About") || strings.ContainsAny(c.WhyNow, "0123456789") {
			t.Errorf("%s: why now shows a number: %q", name, c.WhyNow)
		}
		if !strings.HasPrefix(c.WhyNow, "Urgency not assessed") {
			t.Errorf("%s: why now = %q", name, c.WhyNow)
		}
	}
	zeroRate := cases.SourceFinding{Category: "forecast_wal_runway", Detail: map[string]any{
		"runway_kind": "disk_used", "seconds_to_limit": 3600.0, "rate_per_s": 0.0,
		"samples": 12.0, "span_s": 3600.0, "r2": 0.9}}
	wantUnassessed(t, cases.UrgencyForFinding(zeroRate))
}

func TestUrgency_InvalidNumbersAreUnassessed(t *testing.T) {
	for name, v := range map[string]any{
		"string": "3", "nan": math.NaN(), "+inf": math.Inf(1), "-inf": math.Inf(-1),
		"bool": true, "map": map[string]any{"v": 3},
	} {
		u := cases.UrgencyForFinding(storage(map[string]any{"days_until_full": v,
			"r_squared": 0.9, "covered_days": 30}))
		if u.Assessed || u.TimeToImpactSeconds != nil {
			t.Errorf("%s days_until_full gave %+v", name, u)
		}
		u = cases.UrgencyForFinding(cases.SourceFinding{Category: "forecast_sequence_runway",
			Detail: map[string]any{"runway_kind": "sequence", "seconds_to_limit": v,
				"rate_per_s": 2.0, "samples": 12.0, "span_s": 3600.0}})
		if u.Assessed || u.TimeToImpactSeconds != nil {
			t.Errorf("%s seconds_to_limit gave %+v", name, u)
		}
	}
}

// A runway at or past its limit reports 0 seconds; that is not a
// projection, so it stays unassessed (severity still ranks it).
func TestUrgency_BoundaryOfThePositiveProjection(t *testing.T) {
	for _, secs := range []float64{0, -1, 0.5} {
		u := cases.UrgencyForFinding(cases.SourceFinding{Category: "forecast_wal_runway",
			Detail: map[string]any{"runway_kind": "disk_used", "seconds_to_limit": secs,
				"rate_per_s": 5.0, "samples": 12.0, "span_s": 3600.0, "r2": 0.9}})
		if (secs > 0) != u.Assessed {
			t.Errorf("seconds_to_limit %v assessed = %v", secs, u.Assessed)
		}
	}
	u := cases.UrgencyForFinding(storage(map[string]any{"days_until_full": 1,
		"r_squared": 0.5, "covered_days": 7}))
	wantAssessed(t, u, 86400, "days_until_full 1 from a 7-day regression, R² 0.50")
	u = cases.UrgencyForFinding(storage(map[string]any{"days_until_full": 1,
		"r_squared": 0.49, "covered_days": 7}))
	wantUnassessed(t, u)
}

func TestUrgency_RunwayWithoutAMeasuredR2SaysSo(t *testing.T) {
	u := cases.UrgencyForFinding(cases.SourceFinding{Category: "forecast_sequence_runway",
		Detail: map[string]any{"runway_kind": "sequence", "seconds_to_limit": 7200.0,
			"rate_per_s": 2.0, "samples": 20.0, "span_s": 2700.0, "r2": nil}})
	wantAssessed(t, u, 7200, "seconds_to_limit 7200 from 20 samples over 45 min, R² not measured")
}

func TestUrgency_UnknownCategoryStaysUnassessed(t *testing.T) {
	u := cases.UrgencyForFinding(cases.SourceFinding{Category: "slow_query",
		Detail: map[string]any{"seconds_to_limit": 10.0, "days_until_full": 3}})
	wantUnassessed(t, u)
	if u.Basis != "" {
		t.Fatalf("an unknown category got basis %q", u.Basis)
	}
}

// An overdue table is already past its freeze maximum: the runway has no
// projection (SecondsToLimit NaN), so the case shows headroom only.
func TestUrgency_OverdueTableShowsHeadroom(t *testing.T) {
	f := onlyRunway(t, runway.EvalInput{TablesOK: true, Tables: []probes.WraparoundTable{{
		Relation: "public.orders", XIDAge: 4.5e8, FreezeMaxAge: 2e8,
		MXIDAge: math.NaN(), MXIDFreezeMaxAge: math.NaN()}}})
	c := cases.ProjectFinding(asSource(t, f), nil)
	wantUnassessed(t, c.Urgency)
	if c.Urgency.Basis != "xid_age 450M against a freeze maximum of 200M" {
		t.Fatalf("basis = %q", c.Urgency.Basis)
	}
	if strings.HasPrefix(c.WhyNow, "Urgency not assessed") {
		t.Fatalf("why now hides the headroom: %q", c.WhyNow)
	}
}

// CHECK-24: when the producer fields exist, under 20% of finding cases
// say "Urgency not assessed" (before cases-07 every one of these did).
func TestUrgency_FixtureMostlyAssessed(t *testing.T) {
	disk := runwayTrend(probes.RunwayDiskUsed, probes.SubjectCluster, 48, 6*time.Hour,
		9e10, 1e11, 72*time.Hour, 0.97)
	slot := runwayTrend(probes.RunwayWALSlot, "cdc", 12, time.Hour, 5e9, 10<<30,
		16*time.Hour, 0.95)
	seq := runwayTrend(probes.RunwaySequence, "public.a_seq", 12, time.Hour, 2e9,
		2147483647, 10*24*time.Hour, 1)
	var fs []analyzer.Finding
	for _, in := range []runway.EvalInput{
		{Trends: []probes.RunwayTrend{disk}, TrendsOK: true},
		{Trends: []probes.RunwayTrend{slot}, TrendsOK: true},
		{Trends: []probes.RunwayTrend{seq}, TrendsOK: true, SequencesOK: true,
			Sequences: []probes.SequenceRunway{{Sequence: "public.a_seq"}}},
		{TablesOK: true, Tables: []probes.WraparoundTable{{Relation: "public.t",
			XIDAge: 4.5e8, FreezeMaxAge: 2e8, MXIDAge: math.NaN(),
			MXIDFreezeMaxAge: math.NaN()}}},
	} {
		fs = append(fs, onlyRunway(t, in))
	}
	fs = append(fs, analyzer.Finding{Category: "forecast_disk_growth", Severity: "warning",
		Detail: map[string]any{"days_until_full": 3.0, "r_squared": 0.94, "covered_days": 30.0}})
	fs = append(fs,
		analyzer.Finding{Category: "xid_wraparound", Severity: "warning", Detail: map[string]any{
			"xid_age": 1.2e9, "warning_threshold": 5e8, "critical_threshold": 1.6e9}},
		analyzer.Finding{Category: "sequence_exhaustion", Severity: "warning",
			Detail: map[string]any{"usage_pct": 80.0}},
		analyzer.Finding{Category: "forecast_connection_saturation", Severity: "warning",
			Detail: map[string]any{"days_to_warn_pct": 12.0, "r_squared": 0.9}})
	unassessed := 0
	for _, f := range fs {
		if c := cases.ProjectFinding(asSource(t, f), nil); strings.HasPrefix(c.WhyNow,
			"Urgency not assessed") {
			unassessed++
			t.Logf("%s: %s", f.Category, c.WhyNow)
		}
	}
	if share := float64(unassessed) / float64(len(fs)); len(fs) != 8 || share >= 0.2 {
		t.Fatalf("%d of %d fixture cases are unassessed, want under 20%%", unassessed, len(fs))
	}
}
