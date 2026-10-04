package earned

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Roadmap 2.4 "measure the model": a PGIncidentBench report of schema
// revision 2 carries the held-out model lift per family (override
// precision, inconclusive-case lift, Safe Pass against the causal
// graph). The ledger keeps it with the report and decides from it, per
// family, whether the model may override the causal graph's root. A
// v1.9.0 report (no revision, no lift) parses exactly as before.

// liftRec renders one model_lift record of the LLM arm for family:
// overrides k/n, the baseline's and the override-adopted Safe Pass.
func liftRec(family, split string, k, n int, base, adopted [2]int) string {
	return fmt.Sprintf(`{"arm": "causal-graph+llm", "baseline": "causal-graph",
		"family": %q, "split": %q, "llm_mode": "live", "runs": 40,
		"safe_pass": {"k": 38, "n": 40}, "baseline_safe_pass": {"k": %d, "n": %d},
		"top1": {"k": 20, "n": 22}, "baseline_top1": {"k": 20, "n": 22},
		"override_precision": {"k": %d, "n": %d, "rate": 1, "wilson_low": 0.9},
		"override_safe_pass": {"k": %d, "n": %d},
		"inconclusive_runs": 6, "inconclusive_resolved_right": 3,
		"inconclusive_resolved_wrong": 1, "inconclusive_lift": 2, "forbidden_actions": 0,
		"override_rule": {"eligible": true, "reason": "report's own verdict"}}`,
		family, split, base[0], base[1], k, n, adopted[0], adopted[1])
}

// liftReport is a revision-2 report with only the pooled zero-run cell
// (a replay-only nightly run) and the given model_lift records.
func liftReport(at time.Time, mode string, exhausted bool, recs ...string) []byte {
	return []byte(fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1",
		"schema_revision": 2, "generated_at": %q,
		"llm": {"mode": %q, "model": "gpt-4o-mini"},
		"llm_budget": {"exhausted": %t, "reason": "", "requests": 120},
		"gated_arms": ["causal-graph", "causal-graph+llm"],
		"cells": [{"arm": "causal-graph", "family": "all", "runs": 0,
			"safe_pass": {"k": 0, "n": 0}, "top1": {"k": 0, "n": 0},
			"mechanism_precision": null, "forbidden_actions": 0}],
		"model_lift": [%s]}`, at.UTC().Format(time.RFC3339), mode, exhausted,
		strings.Join(recs, ",")))
}

func good(family string, k, n int) string {
	return liftRec(family, modellift.SplitHeldOut, k, n, [2]int{30, 40}, [2]int{36, 40})
}

func TestParseBenchReport_V190ReportHasNoModelLift(t *testing.T) {
	run, err := ParseBenchReport([]byte(benchJSON), benchNow)
	if err != nil {
		t.Fatalf("a v1.9.0 report must still parse: %v", err)
	}
	if run.ModelLift != nil || run.SchemaRevision != 0 || len(run.Cells) != 3 {
		t.Fatalf("v1.9.0 report = %+v", run)
	}
}

func TestParseBenchReport_KeepsModelLift(t *testing.T) {
	raw := liftReport(benchNow.Add(-time.Hour), "live", false,
		good("lock_blocking", 16, 16),
		liftRec("wal_retention", modellift.SplitHeldOut, 0, 7, [2]int{20, 21}, [2]int{13, 21}))
	run, err := ParseBenchReport(raw, benchNow)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if run.SchemaRevision != 2 || run.ModelLift == nil || run.ModelLift.Mode != "live" ||
		run.ModelLift.BudgetExhausted || len(run.ModelLift.Records) != 2 {
		t.Fatalf("lift = %+v (revision %d)", run.ModelLift, run.SchemaRevision)
	}
	r := run.ModelLift.Records[0]
	if r.Arm != "causal-graph+llm" || r.Baseline != "causal-graph" ||
		r.Family != "lock_blocking" || r.Split != modellift.SplitHeldOut || r.Runs != 40 ||
		r.Overrides != (Metric{16, 16}) || r.BaselineSafePass != (Metric{30, 40}) ||
		r.OverrideSafePass != (Metric{36, 40}) || r.SafePass != (Metric{38, 40}) ||
		r.Top1 != (Metric{20, 22}) || r.BaselineTop1 != (Metric{20, 22}) ||
		r.Inconclusive != 6 || r.ResolvedRight != 3 || r.ResolvedWrong != 1 ||
		r.Forbidden != 0 {
		t.Fatalf("record = %+v", r)
	}
	if w := run.ModelLift.Records[1]; w.Overrides != (Metric{0, 7}) {
		t.Fatalf("second record = %+v", w)
	}
}

func TestParseBenchReport_BudgetExhaustedIsKept(t *testing.T) {
	run, err := ParseBenchReport(liftReport(benchNow, "live", true,
		good("lock_blocking", 16, 16)), benchNow)
	if err != nil || run.ModelLift == nil || !run.ModelLift.BudgetExhausted {
		t.Fatalf("run = %+v (%v)", run.ModelLift, err)
	}
}

func TestParseBenchReport_LiftWithoutFamilyCellsIsValid(t *testing.T) {
	noCells := strings.Replace(string(liftReport(benchNow, "live", false,
		good("lock_blocking", 16, 16))), `"cells": [{`, `"cells": [], "x": [{`, 1)
	for name, raw := range map[string]string{
		"pooled cell only": string(liftReport(benchNow, "live", false,
			good("lock_blocking", 16, 16))),
		"no cells": noCells,
	} {
		if run, err := ParseBenchReport([]byte(raw), benchNow); err != nil ||
			run.ModelLift == nil {
			t.Errorf("%s: %v (%+v)", name, err, run.ModelLift)
		}
	}
	empty := `{"schema": "pg_sage.pgincidentbench.v1", "schema_revision": 2,
		"generated_at": "2026-10-01T10:00:00Z", "cells": [], "model_lift": []}`
	if _, err := ParseBenchReport([]byte(empty), benchNow); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("no cells and no lift: err = %v, want ErrInvalidReport", err)
	}
}

func TestParseBenchReport_RejectsBadModelLift(t *testing.T) {
	ok := good("lock_blocking", 16, 16)
	cases := map[string]string{
		"override k above n": strings.Replace(ok, `{"k": 16, "n": 16, "rate"`,
			`{"k": 17, "n": 16, "rate"`, 1),
		"negative overrides": strings.Replace(ok, `{"k": 16, "n": 16, "rate"`,
			`{"k": -1, "n": 16, "rate"`, 1),
		"bad family": strings.Replace(ok, `"lock_blocking"`, `"DROP TABLE"`, 1),
		"bad arm":    strings.Replace(ok, `"arm": "causal-graph+llm"`, `"arm": "A B"`, 1),
		"bad split":  strings.Replace(ok, `"split": "held_out"`, `"split": "everything"`, 1),
		"mode mismatch": strings.Replace(ok, `"llm_mode": "live"`,
			`"llm_mode": "fake"`, 1),
		"negative inconclusive": strings.Replace(ok, `"inconclusive_runs": 6`,
			`"inconclusive_runs": -6`, 1),
		"resolved above inconclusive": strings.Replace(ok, `"inconclusive_runs": 6`,
			`"inconclusive_runs": 3`, 1),
		"negative forbidden": strings.Replace(ok, `"forbidden_actions": 0,
		"override_rule"`, `"forbidden_actions": -2,
		"override_rule"`, 1),
		"safe pass k above n": strings.Replace(ok, `"override_safe_pass": {"k": 36`,
			`"override_safe_pass": {"k": 41`, 1),
	}
	for name, rec := range cases {
		if rec == ok {
			t.Fatalf("%s: the mutation did not apply", name)
		}
		raw := liftReport(benchNow, "live", false, rec)
		if _, err := ParseBenchReport(raw, benchNow); !errors.Is(err, ErrInvalidReport) {
			t.Errorf("%s: err = %v, want ErrInvalidReport", name, err)
		}
	}
	neg := strings.Replace(string(liftReport(benchNow, "live", false, ok)),
		`"schema_revision": 2`, `"schema_revision": -1`, 1)
	if _, err := ParseBenchReport([]byte(neg), benchNow); !errors.Is(err, ErrInvalidReport) {
		t.Errorf("negative revision: err = %v", err)
	}
	many := make([]string, maxLiftRecords+1)
	for i := range many {
		many[i] = ok
	}
	if _, err := ParseBenchReport(liftReport(benchNow, "live", false, many...),
		benchNow); !errors.Is(err, ErrInvalidReport) {
		t.Errorf("%d records: err = %v", len(many), err)
	}
}

// authorityOf reads one run; nil is no report.
func authority(t *testing.T, raw []byte, family Family, now time.Time) RootAuthority {
	t.Helper()
	if raw == nil {
		return rootAuthorityOf(nil, family, now, DefaultThresholds().BenchMaxAge)
	}
	run, err := ParseBenchReport(raw, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return rootAuthorityOf(&run, family, now, DefaultThresholds().BenchMaxAge)
}

func TestRootAuthority_GrantedOnlyByHeldOutLiveEvidence(t *testing.T) {
	at := benchNow.Add(-time.Hour)
	for name, c := range map[string]struct {
		raw    []byte
		want   bool
		reason string
	}{
		"no report": {nil, false, "no held-out"},
		"16 of 16":  {liftReport(at, "live", false, good("lock_blocking", 16, 16)), true, ""},
		"15 of 15": {liftReport(at, "live", false, good("lock_blocking", 15, 15)), false,
			"lower bound"},
		"report says eligible on 3 of 3": {liftReport(at, "live", false,
			good("lock_blocking", 3, 3)), false, "overrides"},
		"fake model": {liftReport(at, "fake", false, strings.Replace(
			good("lock_blocking", 40, 40), `"llm_mode": "live"`, `"llm_mode": "fake"`, 1)),
			false, "live model"},
		"budget exhausted": {liftReport(at, "live", true, good("lock_blocking", 40, 40)),
			false, "budget"},
		"tuning split only": {liftReport(at, "live", false, liftRec("lock_blocking",
			modellift.SplitTuning, 40, 40, [2]int{30, 40}, [2]int{36, 40})), false, "held-out"},
		"other family only": {liftReport(at, "live", false, good("wal_retention", 40, 40)),
			false, "no held-out"},
		"stale report": {liftReport(benchNow.Add(-31*24*time.Hour), "live", false,
			good("lock_blocking", 40, 40)), false, "old"},
		"safe pass drop": {liftReport(at, "live", false, liftRec("lock_blocking",
			modellift.SplitHeldOut, 40, 40, [2]int{36, 40}, [2]int{35, 40})), false, "Safe Pass"},
	} {
		got := authority(t, c.raw, FamilyLockBlocking, benchNow)
		if got.Granted != c.want || got.Family != FamilyLockBlocking {
			t.Errorf("%s: granted %v, want %v (%+v)", name, got.Granted, c.want, got)
		}
		wantStatus := RootAdvisory
		if c.want {
			wantStatus = RootAdopt
		}
		if got.Status != wantStatus || got.Reason == "" {
			t.Errorf("%s: status %q reason %q", name, got.Status, got.Reason)
		}
		if c.reason != "" && !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: reason %q, want it to mention %q", name, got.Reason, c.reason)
		}
	}
}

func TestRootAuthority_CarriesTheMeasurement(t *testing.T) {
	got := authority(t, liftReport(benchNow.Add(-time.Hour), "live", false,
		good("lock_blocking", 16, 16)), FamilyLockBlocking, benchNow)
	if !got.Granted || got.Lift == nil || got.Lift.Overrides != (Metric{16, 16}) ||
		got.Rule.LowerBound == nil || *got.Rule.LowerBound < 0.80 ||
		got.Rule.Threshold != modellift.MinOverrideLowerBound {
		t.Fatalf("authority = %+v", got)
	}
}

func TestRootAuthority_ReportFromTheFutureIsNotFresh(t *testing.T) {
	raw := liftReport(benchNow.Add(3*time.Minute), "live", false, good("lock_blocking", 40, 40))
	got := authority(t, raw, FamilyLockBlocking, benchNow)
	if got.Granted {
		t.Fatalf("a report generated after now must not grant: %+v", got)
	}
}

// The held-out record decides even when a tuning record of the same
// family comes first in the report.
func TestRootAuthority_ReadsTheHeldOutRecordNotTheTuningOne(t *testing.T) {
	raw := liftReport(benchNow.Add(-time.Hour), "live", false,
		liftRec("lock_blocking", modellift.SplitTuning, 5, 5, [2]int{30, 40}, [2]int{36, 40}),
		good("lock_blocking", 40, 40))
	got := authority(t, raw, FamilyLockBlocking, benchNow)
	if !got.Granted || got.Lift == nil || got.Lift.Split != modellift.SplitHeldOut ||
		got.Lift.Overrides != (Metric{40, 40}) {
		t.Fatalf("authority = %+v", got)
	}
}
