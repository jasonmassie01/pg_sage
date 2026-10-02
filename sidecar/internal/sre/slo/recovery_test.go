package slo

import (
	"testing"
	"time"
)

// Customer recovery from an app SLI (CHECK-22, CHECK-32): recovery is
// certified only by consecutive fresh slices that each carry enough
// traffic and burn below 1x. No data, a counter reset, low traffic or
// traffic that disappeared can never certify recovery.

func slices(n int, bad, eligible float64) []Slice {
	out := make([]Slice, 0, n)
	for i := 0; i < n; i++ {
		start := now.Add(-time.Duration(n-i) * 2 * time.Minute)
		out = append(out, Slice{Start: start, End: start.Add(2 * time.Minute), Bad: bad,
			Eligible: eligible})
	}
	return out
}

func params() RecoveryParams {
	return RecoveryParams{Target: 0.999, MinEligible: 100, MinSlices: 3}
}

func TestRecovery_HealthySlicesCertify(t *testing.T) {
	r := EvaluateRecovery(slices(4, 0, 1000), params())
	if r.State != RecoveryRecovered || r.Reason != "" || r.Evaluated != 3 {
		t.Fatalf("recovery = %+v", r)
	}
}

func TestRecovery_BurningIsNotRecovered(t *testing.T) {
	s := slices(3, 0, 1000)
	s[2].Bad = 2 // 2/1000 = 2x the 0.1% budget
	r := EvaluateRecovery(s, params())
	if r.State != RecoveryNotRecovered || r.Reason != ReasonBurning {
		t.Fatalf("recovery = %+v", r)
	}
	// Exactly at the budget (1x) still burns the budget: not recovered.
	s[2].Bad = 1
	if r := EvaluateRecovery(s, params()); r.State != RecoveryNotRecovered {
		t.Fatalf("1x burn: %+v", r)
	}
}

// CHECK-32: no data, a counter reset and low traffic cannot certify.
func TestRecovery_CannotCertifyWithoutTrustworthyData(t *testing.T) {
	cases := map[string]struct {
		mutate func(s []Slice)
		want   string
	}{
		"no data":       {func(s []Slice) { s[1].Unknown = ReasonNoData }, ReasonNoData},
		"stale":         {func(s []Slice) { s[2].Unknown = ReasonStale }, ReasonStale},
		"counter reset": {func(s []Slice) { s[0].Resets = 1 }, ReasonCounterReset},
		"low traffic":   {func(s []Slice) { s[1].Eligible = 99 }, ReasonLowTraffic},
		"zero traffic":  {func(s []Slice) { s[2].Eligible = 0 }, ReasonZeroEligible},
	}
	for name, c := range cases {
		s := slices(3, 0, 1000)
		c.mutate(s)
		r := EvaluateRecovery(s, params())
		if r.State != RecoveryUnknown || r.Reason != c.want {
			t.Errorf("%s: recovery = %+v, want unknown/%s", name, r, c.want)
		}
	}
}

// CHECK-22: falling traffic alone cannot satisfy the predicate. With a
// pre-incident baseline, a slice under half of it is "traffic_dropped".
func TestRecovery_TrafficThatDisappearedIsUnknown(t *testing.T) {
	p := params()
	p.BaselineEligible = 1000
	s := slices(3, 0, 1000)
	s[2].Eligible = 499
	if r := EvaluateRecovery(s, p); r.State != RecoveryUnknown ||
		r.Reason != ReasonTrafficDropped {
		t.Fatalf("recovery = %+v", r)
	}
	s[2].Eligible = 500 // exactly half: kept
	if r := EvaluateRecovery(s, p); r.State != RecoveryRecovered {
		t.Fatalf("half the baseline: %+v", r)
	}
}

// A clearly burning slice wins over an unknown one: the burn was seen.
func TestRecovery_BurnWinsOverUnknown(t *testing.T) {
	s := slices(3, 0, 1000)
	s[0].Unknown = ReasonNoData
	s[2].Bad = 50
	if r := EvaluateRecovery(s, params()); r.State != RecoveryNotRecovered {
		t.Fatalf("recovery = %+v", r)
	}
}

// Too few slices (or none) cannot certify; only the newest MinSlices
// slices count, so an old burn before recovery does not block it.
func TestRecovery_SliceCount(t *testing.T) {
	if r := EvaluateRecovery(slices(2, 0, 1000), params()); r.State != RecoveryUnknown ||
		r.Reason != ReasonInsufficientSamples {
		t.Fatalf("2 slices: %+v", r)
	}
	if r := EvaluateRecovery(nil, params()); r.State != RecoveryUnknown {
		t.Fatalf("no slices: %+v", r)
	}
	s := slices(5, 0, 1000)
	s[0].Bad = 900
	if r := EvaluateRecovery(s, params()); r.State != RecoveryRecovered {
		t.Fatalf("old burn outside the newest slices: %+v", r)
	}
	p := params()
	p.MinSlices = 0 // zero means the default (3)
	if r := EvaluateRecovery(slices(2, 0, 1000), p); r.State != RecoveryUnknown {
		t.Fatalf("default min slices: %+v", r)
	}
}
