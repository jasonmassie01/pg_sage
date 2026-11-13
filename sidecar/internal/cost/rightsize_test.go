package cost

import (
	"strings"
	"testing"
	"time"
)

// The right-size rule is deterministic and advisory: it needs 14 days of
// hourly rows at 90% coverage of the current size, downsizes one step in
// the family only with headroom (projected p95 <= 60%, max <= 85%, RAM
// for shared_buffers plus used memory) and never onto a burstable class,
// and upsizes on sustained pressure.

var rsNow = time.Date(2026, 11, 20, 12, 30, 0, 0, time.UTC)

const gb = float64(1 << 30)

func f64(v float64) *float64 { return &v }

type hourSpec struct {
	class            string
	vcpus            int
	p95, max, load   float64
	memTotal, memMin float64
}

func r6g2x() hourSpec {
	return hourSpec{class: "db.r6g.2xlarge", vcpus: 8, p95: 25, max: 40, load: 2,
		memTotal: 64 * gb, memMin: 50 * gb}
}

// hoursOf builds n consecutive hourly rows ending in the hour before rsNow.
func hoursOf(n int, s hourSpec) []HourRow {
	last := rsNow.Truncate(time.Hour).Add(-time.Hour)
	rows := make([]HourRow, 0, n)
	for i := n - 1; i >= 0; i-- {
		rows = append(rows, HourRow{Hour: last.Add(-time.Duration(i) * time.Hour),
			InstanceClass: s.class, VCPUs: s.vcpus, MemoryTotalBytes: s.memTotal,
			CPUP95: f64(s.p95), CPUMax: f64(s.max), DBLoadP95: f64(s.load),
			MemAvailableMin: f64(s.memMin), Samples: 60})
	}
	return rows
}

func usdRates() *Rates {
	return &Rates{Source: SourceConfig, Currency: "USD",
		AsOf: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), ComputePerHour: 1.0}
}

func verdictFor(rows []HourRow, sharedBuffers float64, rates *Rates) Verdict {
	return Rightsize(RightsizeInput{Resource: "rds:us-east-1/orders", Rows: rows,
		SharedBuffersBytes: sharedBuffers, Now: rsNow, Rates: rates})
}

func TestRightsize_DownsizesOneStepWithHeadroom(t *testing.T) {
	v := verdictFor(hoursOf(14*24, r6g2x()), 8*gb, usdRates())
	if v.Decision != DecisionDownsize || v.CurrentClass != "db.r6g.2xlarge" ||
		v.TargetClass != "db.r6g.xlarge" {
		t.Fatalf("verdict = %+v, want downsize to db.r6g.xlarge", v)
	}
	if !near(v.ProjectedCPUP95, 50) || !near(v.ProjectedCPUMax, 80) || !near(v.Days, 14) ||
		!near(v.Coverage, 1) {
		t.Fatalf("projection = %+v", v)
	}
	if v.MonthlyDelta == nil || !near(*v.MonthlyDelta, -365) || v.Currency != "USD" {
		t.Fatalf("monthly delta = %v %s, want -365 USD (half the vCPUs)", v.MonthlyDelta,
			v.Currency)
	}
}

func TestRightsize_DaysBoundary(t *testing.T) {
	short := verdictFor(hoursOf(333, r6g2x()), 8*gb, nil) // 13.875 days
	if short.Decision != DecisionUnknown || !strings.Contains(short.Reason, "14 days") {
		t.Fatalf("13.9 days: %+v, want unknown naming 14 days", short)
	}
	full := verdictFor(hoursOf(336, r6g2x()), 8*gb, nil)
	if full.Decision != DecisionDownsize {
		t.Fatalf("14 days: %+v, want downsize", full)
	}
}

// withCoverage keeps rows out of a 400-hour span, dropping from the middle.
func withCoverage(keep int) []HourRow {
	rows := hoursOf(400, r6g2x())
	drop := 400 - keep
	return append(rows[:100:100], rows[100+drop:]...)
}

func TestRightsize_CoverageBoundary(t *testing.T) {
	low := verdictFor(withCoverage(356), 8*gb, nil) // 89%
	if low.Decision != DecisionUnknown || !strings.Contains(low.Reason, "90%") {
		t.Fatalf("89%%: %+v, want unknown naming 90%%", low)
	}
	ok := verdictFor(withCoverage(360), 8*gb, nil) // exactly 90%
	if ok.Decision != DecisionDownsize || !near(ok.Coverage, 0.9) {
		t.Fatalf("90%%: %+v, want downsize", ok)
	}
}

func TestRightsize_ProjectedP95Boundary(t *testing.T) {
	at := r6g2x()
	at.p95 = 30 // 30% x 8/4 = 60%: allowed
	if v := verdictFor(hoursOf(336, at), 8*gb, nil); v.Decision != DecisionDownsize ||
		!near(v.ProjectedCPUP95, 60) {
		t.Fatalf("projected 60%%: %+v, want downsize", v)
	}
	over := r6g2x()
	over.p95 = 30.01
	if v := verdictFor(hoursOf(336, over), 8*gb, nil); v.Decision != DecisionKeep {
		t.Fatalf("projected 60.02%%: %+v, want keep", v)
	}
}

func TestRightsize_ProjectedMaxBoundary(t *testing.T) {
	at := r6g2x()
	at.max = 42.5 // 85% projected: allowed
	if v := verdictFor(hoursOf(336, at), 8*gb, nil); v.Decision != DecisionDownsize {
		t.Fatalf("projected max 85%%: %+v, want downsize", v)
	}
	over := r6g2x()
	over.max = 43
	if v := verdictFor(hoursOf(336, over), 8*gb, nil); v.Decision != DecisionKeep {
		t.Fatalf("projected max 86%%: %+v, want keep", v)
	}
}

func TestRightsize_NeverDownsizesOntoBurstable(t *testing.T) {
	burst := hourSpec{class: "db.t4g.xlarge", vcpus: 4, p95: 5, max: 10, load: 0.1,
		memTotal: 16 * gb, memMin: 14 * gb}
	if v := verdictFor(hoursOf(336, burst), gb, nil); v.Decision != DecisionKeep ||
		v.TargetClass != "" || !strings.Contains(v.Reason, "burstable") {
		t.Fatalf("t4g: %+v, want keep with no burstable target", v)
	}
	smallest := hourSpec{class: "db.m5.large", vcpus: 2, p95: 5, max: 10, load: 0.1,
		memTotal: 8 * gb, memMin: 7 * gb}
	if v := verdictFor(hoursOf(336, smallest), gb, nil); v.Decision != DecisionKeep ||
		v.TargetClass != "" {
		t.Fatalf("m5.large: %+v, want keep (no smaller non-burstable size)", v)
	}
}

func TestRightsize_RAMMustHoldSharedBuffersAndUsedMemory(t *testing.T) {
	// Used memory 14 GiB + shared_buffers 20 GiB = 34 GiB > 32 GiB target.
	if v := verdictFor(hoursOf(336, r6g2x()), 20*gb, nil); v.Decision != DecisionKeep ||
		!strings.Contains(v.Reason, "memory") {
		t.Fatalf("RAM short: %+v, want keep naming memory", v)
	}
	unknownMem := r6g2x()
	unknownMem.memTotal = 0
	if v := verdictFor(hoursOf(336, unknownMem), 8*gb, nil); v.Decision != DecisionKeep {
		t.Fatalf("memory unknown: %+v, want keep (cannot prove the target fits)", v)
	}
}

func TestRightsize_UpsizesOnSustainedCPU(t *testing.T) {
	rows := hoursOf(336, r6g2x())
	hot := func(days int) []HourRow {
		out := append([]HourRow(nil), rows...)
		for d := 0; d < days; d++ { // the newest days, whole days
			for h := 0; h < 24; h++ {
				i := len(out) - 1 - (d*48 + h) // every other day
				out[i].CPUP95, out[i].CPUMax = f64(90), f64(97)
			}
		}
		return out
	}
	if v := verdictFor(hot(3), 8*gb, usdRates()); v.Decision != DecisionUpsize ||
		v.TargetClass != "db.r6g.4xlarge" || v.MonthlyDelta == nil ||
		!near(*v.MonthlyDelta, 730) {
		t.Fatalf("3 of 7 days hot: %+v, want upsize to db.r6g.4xlarge at +730", v)
	}
	if v := verdictFor(hot(2), 8*gb, nil); v.Decision == DecisionUpsize {
		t.Fatalf("2 of 7 days hot: %+v, want no upsize", v)
	}
}

func TestRightsize_UpsizesWhenLoadExceedsVCPUs(t *testing.T) {
	loaded := r6g2x()
	loaded.load = 9
	if v := verdictFor(hoursOf(336, loaded), 8*gb, nil); v.Decision != DecisionUpsize {
		t.Fatalf("db load 9 on 8 vCPUs: %+v, want upsize", v)
	}
	at := r6g2x()
	at.load = 8
	if v := verdictFor(hoursOf(336, at), 8*gb, nil); v.Decision == DecisionUpsize {
		t.Fatalf("db load 8 on 8 vCPUs: %+v, want no upsize (exceeds, not equals)", v)
	}
}

func TestRightsize_UnknownSizeAndServerless(t *testing.T) {
	unknown := r6g2x()
	unknown.class, unknown.vcpus = "db.zz9.large", 0
	if v := verdictFor(hoursOf(336, unknown), 8*gb, nil); v.Decision != DecisionUnknown {
		t.Fatalf("unknown class: %+v, want unknown", v)
	}
	rows := hoursOf(336, r6g2x())
	for i := range rows {
		rows[i].Serverless, rows[i].InstanceClass, rows[i].VCPUs = true, "db.serverless", 0
	}
	v := verdictFor(rows, 8*gb, nil)
	if v.Decision != DecisionUnknown ||
		v.Reason != "unknown: capacity telemetry not collected" {
		t.Fatalf("serverless: %+v, want unknown: capacity telemetry not collected", v)
	}
}

func TestRightsize_EmptyAndStaleRows(t *testing.T) {
	if v := verdictFor(nil, 8*gb, nil); v.Decision != DecisionUnknown {
		t.Fatalf("no rows: %+v, want unknown", v)
	}
	rows := hoursOf(400, r6g2x())
	stale := rows[:len(rows)-72] // telemetry stopped three days ago
	if v := verdictFor(stale, 8*gb, nil); v.Decision != DecisionUnknown ||
		!strings.Contains(v.Reason, "stale") {
		t.Fatalf("stale: %+v, want unknown naming stale telemetry", v)
	}
}

func TestRightsize_ClassChangeEndsTheOldSizesWindow(t *testing.T) {
	old := hoursOf(20*24, hourSpec{class: "db.r6g.4xlarge", vcpus: 16, p95: 10, max: 20,
		load: 1, memTotal: 128 * gb, memMin: 110 * gb})
	cur := hoursOf(10*24, r6g2x())
	rows := append(old[:10*24:10*24], cur...)
	v := verdictFor(rows, 8*gb, nil)
	if v.Decision != DecisionUnknown || v.CurrentClass != "db.r6g.2xlarge" ||
		!near(v.Days, 10) {
		t.Fatalf("after a resize: %+v, want unknown on 10 days of the new size", v)
	}
}

func TestRightsize_NonUSDRatesKeepTheirCurrency(t *testing.T) {
	r := usdRates()
	r.Currency = "EUR"
	v := verdictFor(hoursOf(336, r6g2x()), 8*gb, r)
	if v.MonthlyDelta == nil || !near(*v.MonthlyDelta, -365) || v.Currency != "EUR" {
		t.Fatalf("verdict = %+v, want -365 EUR", v)
	}
	f, usd, ok := RightsizeFinding("rds:us-east-1/orders", v)
	if !ok || usd != nil {
		t.Fatalf("finding ok=%v usd=%v: a EUR delta is never written as USD", ok, usd)
	}
	if f.Detail["currency"] != "EUR" {
		t.Fatalf("detail = %+v, want the currency kept", f.Detail)
	}
}

func TestRightsize_CloudSQLTiers(t *testing.T) {
	for class, want := range map[string][2]string{
		"db-custom-8-32768":  {"db-custom-4-16384", "db-custom-16-65536"},
		"db-custom-2-7680":   {"", "db-custom-4-15360"},
		"db-n1-standard-8":   {"db-n1-standard-4", "db-n1-standard-16"},
		"db-n1-highmem-96":   {"db-n1-highmem-64", ""},
		"db-custom-96-98304": {"db-custom-48-49152", ""},
		"db.r6g.large":       {"", "db.r6g.xlarge"},
		"db.r6g.12xlarge":    {"db.r6g.8xlarge", "db.r6g.16xlarge"},
		"db.t3.medium":       {"db.t3.small", "db.t3.large"},
		"db-f1-micro":        {"", ""},
	} {
		if got := nextSmaller(class); got != want[0] {
			t.Errorf("nextSmaller(%q) = %q, want %q", class, got, want[0])
		}
		if got := nextLarger(class); got != want[1] {
			t.Errorf("nextLarger(%q) = %q, want %q", class, got, want[1])
		}
	}
}

func TestRightsizeFinding_IsAdvisoryInfo(t *testing.T) {
	v := verdictFor(hoursOf(336, r6g2x()), 8*gb, usdRates())
	f, usd, ok := RightsizeFinding("rds:us-east-1/orders", v)
	if !ok || f.Category != CategoryRightsize || f.Severity != "info" ||
		f.RecommendedSQL != "" || f.RollbackSQL != "" ||
		f.ObjectIdentifier != "instance:rds:us-east-1/orders" {
		t.Fatalf("finding = %+v, want an info instance_rightsize finding without SQL", f)
	}
	if usd == nil || !near(*usd, -365) {
		t.Fatalf("usd = %v, want -365", usd)
	}
	for _, key := range []string{"rate_source", "as_of", "target_class", "projected_cpu_p95"} {
		if _, ok := f.Detail[key]; !ok {
			t.Fatalf("detail lacks %s: %+v", key, f.Detail)
		}
	}
	if _, _, ok := RightsizeFinding("x", Verdict{Decision: DecisionKeep}); ok {
		t.Fatal("a keep verdict must not produce a finding")
	}
}
