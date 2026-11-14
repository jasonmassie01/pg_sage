package analyzer

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// No concurrent access tests: the rule functions are pure over one cycle's
// reading, which only the analyzer's cycle goroutine builds and reads.

var tsTestNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func tsExtras(r *tsReading) *RuleExtras {
	return &RuleExtras{ext: &extReading{timescale: r},
		Now: func() time.Time { return tsTestNow }}
}

func strp(s string) *string { return &s }

func TestTimescaleRules_NilReadingEmitsNothing(t *testing.T) {
	for _, ex := range []*RuleExtras{nil, {}, {ext: &extReading{}}} {
		if got := ruleTimescaleJobFailed(nil, nil, nil, ex); got != nil {
			t.Errorf("job failed on %+v = %v, want nil", ex, got)
		}
		if got := ruleTimescaleCaggStale(nil, nil, nil, ex); got != nil {
			t.Errorf("cagg stale on %+v = %v, want nil", ex, got)
		}
	}
}

func TestTimescaleJobFailed_OneFindingPerJob(t *testing.T) {
	next := tsTestNow.Add(30 * time.Minute)
	started := tsTestNow.Add(-time.Hour)
	r := &tsReading{Schema: "timescaledb_information", jobsRead: true, Jobs: []tsJobFailure{
		{JobID: 1000, ProcSchema: "_timescaledb_functions", ProcName: "policy_compression",
			HypertableSchema: strp("public"), HypertableName: strp("metrics"),
			TotalFailures: 4, NextStart: &next, LastRunStartedAt: &started},
		{JobID: 1001, ProcSchema: "public", ProcName: "my_action", TotalFailures: 1},
	}}
	got := ruleTimescaleJobFailed(nil, nil, nil, tsExtras(r))
	if len(got) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(got), got)
	}
	f := got[0]
	if f.Category != categoryTSJobFailed || f.Severity != "warning" ||
		f.ObjectIdentifier != "timescaledb:job:1000" || f.RecommendedSQL != "" ||
		f.ObjectType != "timescaledb_job" {
		t.Fatalf("finding = %+v", f)
	}
	d := f.Detail
	if d["job_id"] != int64(1000) || d["total_failures"] != int64(4) ||
		d["proc_name"] != "policy_compression" || d["hypertable_name"] != "metrics" ||
		d["hypertable_schema"] != "public" ||
		d["next_start"] != next.Format(time.RFC3339) ||
		d["last_run_started_at"] != started.Format(time.RFC3339) {
		t.Fatalf("detail = %+v", d)
	}
	if !strings.Contains(f.Title, "public.metrics") ||
		!strings.Contains(f.Title, "policy_compression") {
		t.Fatalf("title %q does not name the hypertable and policy", f.Title)
	}
	if !strings.Contains(f.Recommendation, "run_job(1000)") ||
		!strings.Contains(f.Recommendation, "policy_compression") {
		t.Fatalf("recommendation %q", f.Recommendation)
	}
	g := got[1]
	if _, ok := g.Detail["hypertable_name"]; ok {
		t.Fatalf("a job without a hypertable has detail.hypertable_name: %+v", g.Detail)
	}
	if g.Detail["next_start"] != nil || g.ObjectIdentifier != "timescaledb:job:1001" ||
		!strings.Contains(g.Title, "public.my_action") {
		t.Fatalf("second finding = %+v", g)
	}
}

func TestTimescaleJobFailed_QuotesOddNames(t *testing.T) {
	r := &tsReading{Schema: "timescaledb_information", jobsRead: true, Jobs: []tsJobFailure{
		{JobID: 5, ProcSchema: "_timescaledb_functions", ProcName: "policy_retention",
			HypertableSchema: strp(`My "ts"`), HypertableName: strp("Metrics"),
			TotalFailures: 2},
	}}
	got := ruleTimescaleJobFailed(nil, nil, nil, tsExtras(r))
	if len(got) != 1 || !strings.Contains(got[0].Title, `"My ""ts"""."Metrics"`) {
		t.Fatalf("finding = %+v", got)
	}
}

func TestTimescaleJobFailed_NotReadEmitsNothing(t *testing.T) {
	r := &tsReading{Schema: "timescaledb_information",
		Jobs: []tsJobFailure{{JobID: 1, TotalFailures: 2}}}
	if got := ruleTimescaleJobFailed(nil, nil, nil, tsExtras(r)); len(got) != 0 {
		t.Fatalf("an unread job list emitted %v", got)
	}
}

func tsStaleReading(lag, allowed time.Duration) *tsReading {
	return &tsReading{Schema: "timescaledb_information", caggsRead: true,
		WatermarkSchema: "_timescaledb_functions", Caggs: []tsCagg{{
			ViewSchema: "public", ViewName: "metrics_hourly", HypertableSchema: "public",
			HypertableName: "metrics", JobID: 1002, ScheduleInterval: "01:00:00",
			EndOffset: "00:30:00", AllowedLag: allowed, Watermark: tsTestNow.Add(-lag),
		}}}
}

func TestTimescaleCaggStale_LagBoundary(t *testing.T) {
	allowed := 90 * time.Minute
	limit := tsCaggLagFactor * allowed
	cases := []struct {
		lag  time.Duration
		want int
	}{{0, 0}, {-time.Hour, 0}, {limit - time.Second, 0}, {limit, 0},
		{limit + time.Second, 1}, {48 * time.Hour, 1}}
	for _, c := range cases {
		got := ruleTimescaleCaggStale(nil, nil, nil, tsExtras(tsStaleReading(c.lag, allowed)))
		if len(got) != c.want {
			t.Fatalf("lag %v (limit %v): findings = %d, want %d", c.lag, limit,
				len(got), c.want)
		}
		if c.want == 0 {
			continue
		}
		f := got[0]
		if f.Category != categoryTSCaggStale || f.Severity != "warning" ||
			f.ObjectIdentifier != "public.metrics_hourly" || f.RecommendedSQL != "" ||
			f.ObjectType != "continuous_aggregate" {
			t.Fatalf("finding = %+v", f)
		}
		d := f.Detail
		if d["lag_seconds"] != int64(c.lag/time.Second) ||
			d["threshold_seconds"] != int64(limit/time.Second) ||
			d["job_id"] != int64(1002) || d["schedule_interval"] != "01:00:00" ||
			d["end_offset"] != "00:30:00" ||
			d["watermark"] != tsTestNow.Add(-c.lag).Format(time.RFC3339) ||
			d["watermark_function"] != "_timescaledb_functions.cagg_watermark" {
			t.Fatalf("detail = %+v", d)
		}
		if !strings.Contains(f.Recommendation, "run_job(1002)") ||
			!strings.Contains(f.Recommendation, "policy_refresh_continuous_aggregate") {
			t.Fatalf("recommendation %q", f.Recommendation)
		}
	}
}

func TestTimescaleCaggStale_ZeroAllowedLagNotJudged(t *testing.T) {
	for _, allowed := range []time.Duration{0, -time.Minute} {
		r := tsStaleReading(1000*time.Hour, allowed)
		if got := ruleTimescaleCaggStale(nil, nil, nil, tsExtras(r)); len(got) != 0 {
			t.Fatalf("allowed lag %v emitted %v", allowed, got)
		}
	}
}

func TestTimescaleCaggStale_NotReadEmitsNothing(t *testing.T) {
	r := tsStaleReading(1000*time.Hour, time.Hour)
	r.caggsRead = false
	if got := ruleTimescaleCaggStale(nil, nil, nil, tsExtras(r)); len(got) != 0 {
		t.Fatalf("an unread cagg list emitted %v", got)
	}
}

func TestTimescaleCaggStale_QuotedViewIdentity(t *testing.T) {
	r := tsStaleReading(1000*time.Hour, time.Hour)
	r.Caggs[0].ViewSchema, r.Caggs[0].ViewName = `My "ts"`, "Hourly"
	got := ruleTimescaleCaggStale(nil, nil, nil, tsExtras(r))
	if len(got) != 1 || got[0].ObjectIdentifier != `"My ""ts"""."Hourly"` {
		t.Fatalf("finding = %+v", got)
	}
}

func TestTimescaleWatermarkTime(t *testing.T) {
	at := tsTestNow.Add(-time.Hour)
	if got, ok := tsWatermarkTime(at.UnixMicro()); !ok || !got.Equal(at) {
		t.Fatalf("watermark of %v = %v, %v", at, got, ok)
	}
	// TimescaleDB returns the time type's minimum for a cagg that never
	// materialized anything; it is not a lag to judge.
	for _, us := range []int64{-210866803200000000, tsMinWatermarkMicros - 1} {
		if _, ok := tsWatermarkTime(us); ok {
			t.Fatalf("watermark %d judged as materialized", us)
		}
	}
	if _, ok := tsWatermarkTime(tsMinWatermarkMicros); !ok {
		t.Fatal("the first year-1 microsecond was treated as never materialized")
	}
}

// The two TimescaleDB categories are rules a rule pack can name, and the
// gate asks for the extension.
func TestTimescaleRules_RegisteredAndDisableable(t *testing.T) {
	names := map[string]bool{}
	for _, spec := range AllRules {
		names[spec.Name] = true
	}
	for _, c := range tsCategories {
		if !names[c] {
			t.Errorf("rule %s is not in AllRules", c)
		}
		d, err := findingcat.ResolveDisabled([]string{c})
		if err != nil || !d.Rules[c] || !d.Categories[c] {
			t.Errorf("ResolveDisabled(%s) = %+v, %v", c, d, err)
		}
		if !slices.Contains(Categories(), c) {
			t.Errorf("%s is not an analyzer category", c)
		}
		if !isExtCategory(c) {
			t.Errorf("%s is not an extension category", c)
		}
	}
	if len(tsCategories) != 2 {
		t.Fatalf("tsCategories = %v, want the two TimescaleDB categories", tsCategories)
	}
	if !slices.Contains(extGateNames, extTimescale) ||
		!strings.Contains(extGateSQL, "'timescaledb' THEN 'job_stats'") {
		t.Fatalf("the gate does not look for TimescaleDB: %v", extGateNames)
	}
}

// The watermark stops with the source's data, so a policy that succeeded
// within the threshold clears the aggregate however far the watermark lags.
func TestTimescaleCaggStale_RecentPolicySuccessIsClear(t *testing.T) {
	allowed := 90 * time.Minute
	limit := tsCaggLagFactor * allowed
	at := func(d time.Duration) *time.Time { v := tsTestNow.Add(-d); return &v }
	cases := []struct {
		last *time.Time
		want int
	}{{nil, 1}, {at(0), 0}, {at(limit), 0}, {at(limit + time.Second), 1},
		{at(-time.Hour), 0}}
	for _, c := range cases {
		r := tsStaleReading(48*time.Hour, allowed)
		r.Caggs[0].LastSuccess = c.last
		got := ruleTimescaleCaggStale(nil, nil, nil, tsExtras(r))
		if len(got) != c.want {
			t.Fatalf("last success %v (limit %v): findings = %d, want %d", c.last, limit,
				len(got), c.want)
		}
		if c.want == 0 {
			continue
		}
		v, ok := got[0].Detail["last_successful_finish"]
		if c.last == nil && ok {
			t.Fatalf("detail names a success that never happened: %v", v)
		}
		if c.last != nil && v != c.last.Format(time.RFC3339) {
			t.Fatalf("last_successful_finish = %v, want %v", v, c.last)
		}
	}
}
