package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/review/gen/capacity"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-03: how a census measurement is read from review-02f's day
// calls (full members) and from the sweeper's readings (observe-only).
// Pure functions: no DB, no concurrency.

func days(from time.Time, calls ...int64) []capacity.DayCalls {
	out := make([]capacity.DayCalls, len(calls))
	for i, c := range calls {
		out[i] = capacity.DayCalls{Day: from.AddDate(0, 0, i), Observed: c >= 0,
			WorkloadCalls: max(c, 0)}
	}
	return out
}

func TestCensusFromDays_UsesTheIdleRule(t *testing.T) {
	d0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// -1 is an unobserved day: it ends the observed run.
	m := censusFromDays(days(d0, 5, -1, 0, 3, 0, 0, 0))
	if m.Unmeasured != "" || m.LastActivityAt == nil ||
		!m.LastActivityAt.Equal(d0.AddDate(0, 0, 3)) || m.QuietFor != 3*day ||
		m.ObservedFrom == nil || !m.ObservedFrom.Equal(d0.AddDate(0, 0, 2)) ||
		m.ObservedTo == nil || !m.ObservedTo.Equal(d0.AddDate(0, 0, 7)) ||
		!strings.Contains(m.Basis, "pg_sage's own statements excluded") {
		t.Fatalf("measure %+v", m)
	}
	if q := capacity.QuietDays(days(d0, 5, -1, 0, 3, 0, 0, 0)); m.QuietFor != time.Duration(q)*day {
		t.Fatalf("quiet %s, the idle rule says %d days", m.QuietFor, q)
	}
	quiet := censusFromDays(days(d0, 0, 0, 0))
	if quiet.LastActivityAt != nil || quiet.QuietFor != 3*day ||
		!quiet.ObservedFrom.Equal(d0) {
		t.Fatalf("quiet measure %+v", quiet)
	}
	for _, empty := range [][]capacity.DayCalls{nil, {}, days(d0, -1, -1)} {
		if got := censusFromDays(empty); got.Unmeasured == "" || got.QuietFor != 0 {
			t.Fatalf("no history %v: %+v, want unmeasured", empty, got)
		}
	}
	// The newest day unobserved: the idle rule counts no quiet days.
	if got := censusFromDays(days(d0, 0, 0, -1)); got.QuietFor != 0 ||
		got.ObservedFrom == nil || !got.ObservedFrom.Equal(d0) ||
		got.ObservedTo == nil || !got.ObservedTo.Equal(d0.AddDate(0, 0, 2)) {
		t.Fatalf("lagging newest day %+v", got)
	}
}

func TestCensusFromReading(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	read := base.Add(50 * day)
	r := CensusActivity{Found: true, WorkloadCalls: ip(10), SizeBytes: ip(8192),
		BaselineAt: base, ReadAt: read}
	m := censusFromReading(r)
	if m.Unmeasured != "" || m.LastActivityAt != nil || m.QuietFor != 50*day ||
		!m.ObservedFrom.Equal(base) || m.ObservedTo == nil || !m.ObservedTo.Equal(read) ||
		*m.SizeBytes != 8192 {
		t.Fatalf("quiet reading %+v", m)
	}
	r.LastActivityAt = tp(base.Add(45 * day))
	if m := censusFromReading(r); m.QuietFor != 5*day || !m.LastActivityAt.Equal(
		base.Add(45*day)) {
		t.Fatalf("active reading %+v", m)
	}
	if m := censusFromReading(CensusActivity{}); !strings.Contains(m.Unmeasured,
		"not swept") {
		t.Fatalf("no reading %+v", m)
	}
	r.WorkloadCalls = nil
	if m := censusFromReading(r); !strings.Contains(m.Unmeasured, "pg_stat_statements") ||
		m.SizeBytes == nil {
		t.Fatalf("no pg_stat_statements %+v, want unmeasured with the size kept", m)
	}
}

// fakeActivity answers the observe-only readings by key.
type fakeActivity map[string]CensusActivity

func (f fakeActivity) CensusActivity(_ context.Context, key string) (CensusActivity,
	error) {
	if key == "fails" {
		return CensusActivity{}, errors.New("meta read failed")
	}
	return f[key], nil
}

func TestCensusMeasure_DispatchesByKind(t *testing.T) {
	base := censusNow.Add(-40 * day)
	act := fakeActivity{
		"obs":  {Found: true, WorkloadCalls: ip(1), BaselineAt: base, ReadAt: censusNow},
		"down": {Found: true, SweepStatus: store.ObserveUnreachable, SweepError: "refused"},
	}
	measure := NewCensusMeasure(func(string) *DatabaseInstance { return nil }, act,
		func() time.Time { return censusNow })
	ctx := context.Background()
	m, err := measure(ctx, CensusSubject{Key: "obs", Kind: CensusObserveOnly})
	if err != nil || m.QuietFor != 40*day {
		t.Fatalf("observe-only %+v %v", m, err)
	}
	if _, err := measure(ctx, CensusSubject{Key: "down", Kind: CensusObserveOnly}); !errors.Is(err,
		ErrCensusUnreachable) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("unreachable sweep: %v", err)
	}
	if _, err := measure(ctx, CensusSubject{Key: "fails", Kind: CensusObserveOnly}); err == nil ||
		errors.Is(err, ErrCensusUnreachable) || !strings.Contains(err.Error(), "meta read") {
		t.Fatalf("meta failure: %v", err)
	}
	if _, err := measure(ctx, CensusSubject{Key: "full", Kind: CensusFull}); !errors.Is(err,
		ErrCensusUnreachable) {
		t.Fatalf("full member without a runtime: %v, want unreachable", err)
	}
	if _, err := measure(ctx, CensusSubject{Key: "x", Kind: "branch"}); err == nil {
		t.Fatal("an unknown kind was measured")
	}
}
