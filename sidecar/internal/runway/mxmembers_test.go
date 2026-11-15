package runway

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
)

// The multixact member space arithmetic (guard-02a), pinned against hand-
// computed examples.
// No concurrent access tests here: these functions are pure; the
// samplers' concurrency is in mxmembers_db_test.go.

func TestMXMembers_LiveMultixacts(t *testing.T) {
	cases := []struct {
		name         string
		oldest, next uint32
		want         uint32
	}{
		{"fresh cluster", 1, 1, 0},
		{"one hundred", 1, 101, 100},
		{"wraps past 0, which is skipped", 4294967290, 10, 15},
		{"next precedes oldest", 100, 50, 0},
		{"next just before oldest", 2, 1, 0},
	}
	for _, c := range cases {
		if got := liveMultixacts(c.oldest, c.next); got != c.want {
			t.Errorf("%s: liveMultixacts(%d, %d) = %d, want %d", c.name, c.oldest, c.next,
				got, c.want)
		}
	}
}

func TestMXMembers_SampleIDs(t *testing.T) {
	if got := sampleIDs(7, 0, MXMemberSampleMax); len(got) != 0 {
		t.Fatalf("no live multixacts sampled %v", got)
	}
	if got := sampleIDs(7, 5, 0); len(got) != 0 {
		t.Fatalf("max 0 sampled %v", got)
	}
	got, want := sampleIDs(7, 5, MXMemberSampleMax), []int64{7, 8, 9, 10, 11}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("census = %v, want %v", got, want)
	}
	got = sampleIDs(1, 1000, MXMemberSampleMax)
	if len(got) != MXMemberSampleMax || got[0] != 1 || got[1] != 6 || got[199] != 996 {
		t.Fatalf("1000 live: %d ids, first %d, second %d, last %d; want 200, 1, 6, 996",
			len(got), got[0], got[1], got[len(got)-1])
	}
	wrapped := sampleIDs(4294967294, liveMultixacts(4294967294, 3), MXMemberSampleMax)
	if want := []int64{4294967294, 4294967295, 1, 2}; !reflect.DeepEqual(wrapped, want) {
		t.Fatalf("wrapped ids = %v, want %v (0 is never an id)", wrapped, want)
	}
}

func TestMXMembers_EstimateArithmetic(t *testing.T) {
	counts := make([]int64, 0, 200)
	for i := 0; i < 100; i++ {
		counts = append(counts, 2, 4)
	}
	mean, total, se := estimateMembers(1000, counts)
	// s^2 = 200/199; se = 1000 * sqrt(s^2 / 200 * (800/999)) = 63.436.
	if mean != 3 || total != 3000 || math.Abs(se-63.436) > 0.001 {
		t.Fatalf("estimate = %v, %v, %v; want 3, 3000, 63.436", mean, total, se)
	}
	mean, total, se = estimateMembers(4, []int64{1, 2, 3, 4})
	if mean != 2.5 || total != 10 || se != 0 {
		t.Fatalf("census = %v, %v, %v; want 2.5, 10 and no error", mean, total, se)
	}
	for _, c := range []struct {
		live   uint32
		counts []int64
	}{{0, nil}, {0, []int64{3}}, {10, nil}, {10, []int64{}}} {
		mean, total, se := estimateMembers(c.live, c.counts)
		if mean != 0 || total != 0 || se != 0 {
			t.Errorf("estimateMembers(%d, %v) = %v, %v, %v; want zeros", c.live, c.counts,
				mean, total, se)
		}
	}
}

func TestMXMembers_OffsetDeltaWrapsModulo2To32(t *testing.T) {
	if d, ok := offsetDelta(4294967000, 200); !ok || d != 496 {
		t.Fatalf("delta across wraparound = %d, %v; want 496, true", d, ok)
	}
	if d, ok := offsetDelta(500, 500); !ok || d != 0 {
		t.Fatalf("no change = %d, %v; want 0, true", d, ok)
	}
	if _, ok := offsetDelta(1000, 900); ok {
		t.Fatal("an offset that went back was taken as a rate, want a reset")
	}
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	prev := MXControl{NextOffset: 4294967000, CheckpointAt: t0}
	cur := MXControl{NextOffset: 200, CheckpointAt: t0.Add(10 * time.Second)}
	if r, ok := memberRate(prev, cur); !ok || math.Abs(r-49.6) > 1e-9 {
		t.Fatalf("rate across wraparound = %v, %v; want 49.6, true", r, ok)
	}
	for name, at := range map[string]time.Time{"same checkpoint": t0,
		"earlier checkpoint": t0.Add(-time.Second)} {
		if r, ok := memberRate(prev, MXControl{NextOffset: 200, CheckpointAt: at}); ok {
			t.Errorf("%s: rate %v, want none", name, r)
		}
	}
	if r, ok := memberRate(MXControl{NextOffset: 1000, CheckpointAt: t0},
		MXControl{NextOffset: 900, CheckpointAt: t0.Add(time.Hour)}); ok {
		t.Fatalf("a reset gave rate %v, want none (never a negative rate)", r)
	}
}

func TestMXMembers_RunwayBoundary(t *testing.T) {
	day := 86400.0
	cases := []struct {
		name    string
		members float64
		want    string
	}{
		{"exactly 14 days", MXMemberSafeThreshold - 14*day, SeverityWarning},
		{"14 days and 1 s", MXMemberSafeThreshold - 14*day - 1, ""},
		{"exactly 3 days", MXMemberSafeThreshold - 3*day, SeverityCritical},
		{"3 days and 1 s", MXMemberSafeThreshold - 3*day - 1, SeverityWarning},
		{"at the threshold", MXMemberSafeThreshold, SeverityCritical},
	}
	for _, c := range cases {
		r := mxRunway(MXMemberEstimate{Members: c.members}, 1, true, defaultMXLimits)
		if r.Severity != c.want {
			t.Errorf("%s: severity %q (%v s), want %q", c.name, r.Severity, r.SecondsToSafe,
				c.want)
		}
	}
	r := mxRunway(MXMemberEstimate{Members: MXMemberSafeThreshold - 14*day}, 1, true,
		defaultMXLimits)
	if r.SecondsToSafe != 14*day || r.SecondsToHard != MXMemberHardLimit-r.Upper {
		t.Fatalf("seconds to safe %v, hard %v; want %v and %v", r.SecondsToSafe,
			r.SecondsToHard, 14*day, MXMemberHardLimit-r.Upper)
	}
}

func TestMXMembers_RunwayIsProjectedFromTheUpperBound(t *testing.T) {
	e := MXMemberEstimate{Members: MXMemberSafeThreshold - 2e6, StdErr: 5e5}
	r := mxRunway(e, 1, true, defaultMXLimits)
	// Upper = estimate + 2 x 5e5 = safe - 1e6: 1e6 s (11.6 days), a
	// warning; the point estimate alone (23 days) would be clean.
	if r.Upper != MXMemberSafeThreshold-1e6 || r.SecondsToSafe != 1e6 ||
		r.Severity != SeverityWarning {
		t.Fatalf("runway = %+v, want upper safe-1e6, 1e6 s, warning", r)
	}
	clipped := mxRunway(MXMemberEstimate{Members: MXMemberSpace, StdErr: 1e9}, 1, true,
		defaultMXLimits)
	if clipped.Upper != MXMemberSpace || clipped.SecondsToHard != 0 {
		t.Fatalf("upper %v, hard %v; want the space and 0", clipped.Upper,
			clipped.SecondsToHard)
	}
}

func TestMXMembers_RunwayWithoutARate(t *testing.T) {
	past := mxRunway(MXMemberEstimate{Members: MXMemberSafeThreshold + 1}, 0, false,
		defaultMXLimits)
	if past.SecondsToSafe != 0 || past.Severity != SeverityCritical ||
		!math.IsNaN(past.SecondsToHard) || !math.IsNaN(past.RatePerS) {
		t.Fatalf("past the threshold without a rate = %+v, want critical at 0 s", past)
	}
	low := mxRunway(MXMemberEstimate{Members: 1000}, 0, false, defaultMXLimits)
	if !math.IsNaN(low.SecondsToSafe) || low.Severity != "" {
		t.Fatalf("no rate = %+v, want unknown runway and no finding", low)
	}
	idle := mxRunway(MXMemberEstimate{Members: 1000}, 0, true, defaultMXLimits)
	if !math.IsInf(idle.SecondsToSafe, 1) || idle.Severity != "" {
		t.Fatalf("zero rate = %+v, want an infinite runway and no finding", idle)
	}
	empty := mxRunway(MXMemberEstimate{}, 5, true, defaultMXLimits)
	if empty.Upper != 0 || empty.Severity != "" {
		t.Fatalf("no multixacts = %+v, want zero members and no finding", empty)
	}
}

func TestMXMembers_FindingCarriesTheEstimateAndItsError(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	e := MXMemberEstimate{MXControl: MXControl{NextMXID: 501, NextOffset: 9000,
		OldestMXID: 1, CheckpointAt: at}, Status: MXMeasured, Live: 500, Sampled: 200,
		MeanMembers: 3, Members: MXMemberSafeThreshold - 864000, StdErr: 0}
	r := mxRunway(e, 2, true, defaultMXLimits)
	f := mxFinding(e, r, defaultMXLimits, clusterScope{})
	if f.Category != CategoryMXMembers || f.ObjectIdentifier != MXMembersIdentifier ||
		f.Severity != SeverityWarning || f.RecommendedSQL != "" || f.ActionRisk != "safe" {
		t.Fatalf("finding = %+v", f)
	}
	d := f.Detail
	if d["seconds_to_limit"] != 432000.0 || d["rate_per_s"] != 2.0 ||
		d["members_est"] != round2(e.Members) || d["members_stderr"] != 0.0 ||
		d["limit"] != MXMemberSafeThreshold || d["live_multixacts"] != uint32(500) ||
		d["runway_kind"] != RunwayMXMembers || d["checkpoint_at"] != "2026-11-15T12:00:00Z" {
		t.Fatalf("detail = %v", d)
	}
	if _, ok := d["scope"]; ok {
		t.Fatalf("a single database's finding claims a scope: %v", d)
	}
	if !strings.Contains(f.Title, "aggressive-vacuum threshold") ||
		!strings.Contains(f.Title, "in about 120h0m0s") {
		t.Fatalf("title = %q", f.Title)
	}
	if !strings.Contains(f.Recommendation, "VACUUM (FREEZE)") {
		t.Fatalf("recommendation = %q", f.Recommendation)
	}
	shared := mxFinding(e, r, defaultMXLimits, clusterScope{shared: true, isOwner: true,
		key: "k", owner: "db", members: []string{"db"}})
	if shared.Detail["scope"] != ScopeCluster {
		t.Fatalf("an owner's finding does not say it is about the cluster: %v",
			shared.Detail)
	}
}

func TestMXMembers_OptionsValidateTheInterval(t *testing.T) {
	o := testOptions()
	o.Interval, o.Retention = time.Minute, 48*time.Hour
	for _, bad := range []time.Duration{-time.Second, 30 * time.Second} {
		o.MXMemberInterval = bad
		if err := o.validate(); err == nil ||
			!strings.Contains(err.Error(), "multixact member interval") {
			t.Errorf("interval %s validated: %v", bad, err)
		}
	}
	for _, ok := range []time.Duration{0, time.Minute, DefaultMXMemberInterval} {
		o.MXMemberInterval = ok
		if err := o.validate(); err != nil {
			t.Errorf("interval %s rejected: %v", ok, err)
		}
	}
}

func mxUrgency(t *testing.T, f analyzer.Finding) cases.Urgency {
	t.Helper()
	raw, err := json.Marshal(f.Detail) // as sage.findings.detail stores it
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return cases.UrgencyForFinding(cases.SourceFinding{Category: f.Category,
		Severity: cases.Severity(f.Severity), Detail: detail})
}

func TestMXMembers_FindingUrgencyIsAssessed(t *testing.T) {
	e := MXMemberEstimate{Status: MXMeasured, Members: MXMemberSafeThreshold - 864000}
	u := mxUrgency(t, mxFinding(e, mxRunway(e, 2, true, defaultMXLimits), defaultMXLimits,
		clusterScope{}))
	if !u.Assessed || u.TimeToImpactSeconds == nil || *u.TimeToImpactSeconds != 432000 ||
		u.Impact != "until aggressive vacuuming of multixact member space starts" {
		t.Fatalf("urgency = %+v, want 432000 s to the aggressive-vacuum threshold", u)
	}
	past := MXMemberEstimate{Status: MXMeasured, Members: MXMemberSafeThreshold}
	u = mxUrgency(t, mxFinding(past, mxRunway(past, 0, false, defaultMXLimits),
		defaultMXLimits, clusterScope{}))
	if u.Assessed || !u.LimitReached {
		t.Fatalf("past the threshold: urgency %+v, want the limit reached", u)
	}
}
