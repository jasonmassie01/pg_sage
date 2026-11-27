package executor

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-01: a decision the gate cannot record is counted on every
// failure (pg_sage_policy_record_failures_total) and logged once per
// decision fingerprint per hour, so a failing candidate cannot flood the
// log every cycle and cannot fail silently either.

func TestRecordFailureLogAllowsOncePerHourPerKey(t *testing.T) {
	var limiter recordFailureLog // the zero value is ready to use
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	steps := []struct {
		key  string
		at   time.Time
		want bool
	}{
		{"fp_a", t0, true},
		{"fp_a", t0.Add(time.Second), false},
		{"fp_a", t0.Add(time.Hour - time.Nanosecond), false},
		{"fp_b", t0.Add(time.Minute), true},
		{"fp_a", t0.Add(time.Hour), true}, // boundary: a full hour later logs again
		{"fp_a", t0.Add(time.Hour + time.Minute), false},
	}
	for i, step := range steps {
		if got := limiter.allow(step.key, step.at); got != step.want {
			t.Fatalf("step %d allow(%s, +%s) = %v, want %v",
				i, step.key, step.at.Sub(t0), got, step.want)
		}
	}
}

// Keys whose hour has passed are pruned, so a stream of distinct failing
// fingerprints cannot grow the limiter without bound.
func TestRecordFailureLogPrunesExpiredKeys(t *testing.T) {
	var limiter recordFailureLog
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		limiter.allow(fmt.Sprintf("fp_%d", i), t0)
	}
	if !limiter.allow("fp_late", t0.Add(2*time.Hour)) {
		t.Fatal("a new key must log")
	}
	if n := limiter.size(); n != 1 {
		t.Fatalf("limiter holds %d keys after the hour, want 1", n)
	}
}

type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) logf(_ string, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *capturedLog) matching(substr string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range c.lines {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

func recordFailureCount(database, reason string) int64 {
	for _, c := range RecordFailureCounts() {
		if c.Database == database && c.Reason == reason {
			return c.Count
		}
	}
	return 0
}

func parkedContractless(target string) (policy.ActionRequest, policy.Decision) {
	req := policy.ActionRequest{Feature: "vacuum", SQL: "VACUUM FULL " + target,
		TargetObjs: []string{target}}
	return req, policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonNoTypedContract, RiskTier: policy.RiskHigh}
}

func TestReportRecordFailureCountsEveryFailureAndLogsHourly(t *testing.T) {
	logs := &capturedLog{}
	database := fmt.Sprintf("recfail_%d", time.Now().UnixNano())
	exec := New(nil, &config.Config{}, time.Time{}, logs.logf)
	exec.WithDatabaseName(database)
	req, decision := parkedContractless("public.orders")
	recordErr := errors.New(`violates check constraint "decision_risk_tier_check"`)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	exec.reportRecordFailureAt(req, decision, recordErr, t0)
	exec.reportRecordFailureAt(req, decision, recordErr, t0.Add(10*time.Minute))
	if got := recordFailureCount(database, "no_typed_contract"); got != 2 {
		t.Fatalf("record failures = %d, want 2 (every failure counts)", got)
	}
	lines := logs.matching("could not record")
	if len(lines) != 1 {
		t.Fatalf("logged %d lines within the hour, want 1: %v", len(lines), lines)
	}
	for _, want := range []string{"parked/no_typed_contract", "public.orders",
		"decision_risk_tier_check", "policy_unavailable", database} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("log %q lacks %q", lines[0], want)
		}
	}
	exec.reportRecordFailureAt(req, decision, recordErr, t0.Add(time.Hour))
	if n := len(logs.matching("could not record")); n != 2 {
		t.Fatalf("logged %d lines after the hour, want 2", n)
	}
	other, _ := parkedContractless("public.items")
	exec.reportRecordFailureAt(other, decision, recordErr, t0.Add(time.Hour))
	if n := len(logs.matching("public.items")); n != 1 {
		t.Fatalf("a different decision must log on its own; got %d lines", n)
	}
}

// An embedder's executor has no database name: it still logs, but exports
// no series (every series carries its database, as the park counter does).
func TestReportRecordFailureWithoutDatabaseNameLogsOnly(t *testing.T) {
	logs := &capturedLog{}
	exec := New(nil, &config.Config{}, time.Time{}, logs.logf)
	req, decision := parkedContractless("public.nameless")
	before := len(RecordFailureCounts())
	exec.reportRecordFailureAt(req, decision, errors.New("boom"), time.Now())
	if len(logs.matching("public.nameless")) != 1 {
		t.Fatalf("logs = %v, want one record-failure line", logs.lines)
	}
	if recordFailureCount("", "no_typed_contract") != 0 || len(RecordFailureCounts()) != before {
		t.Fatalf("an unnamed executor exported a series: %v", RecordFailureCounts())
	}
}

func TestReportRecordFailureConcurrently(t *testing.T) {
	const workers = 32
	logs := &capturedLog{}
	database := fmt.Sprintf("recfail_conc_%d", time.Now().UnixNano())
	exec := New(nil, &config.Config{}, time.Time{}, logs.logf)
	exec.WithDatabaseName(database)
	req, decision := parkedContractless("public.orders")
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exec.reportRecordFailureAt(req, decision, errors.New("boom"), now)
		}()
	}
	wg.Wait()
	if got := recordFailureCount(database, "no_typed_contract"); got != workers {
		t.Fatalf("record failures = %d, want %d", got, workers)
	}
	if n := len(logs.matching("could not record")); n != 1 {
		t.Fatalf("logged %d lines, want 1", n)
	}
}

func TestRecordFailureCountsAreSorted(t *testing.T) {
	prefix := fmt.Sprintf("recfail_sort_%d", time.Now().UnixNano())
	countRecordFailure(prefix+"_b", "no_typed_contract")
	countRecordFailure(prefix+"_a", "policy_unavailable")
	countRecordFailure(prefix+"_a", "executor_disabled")
	var got []string
	for _, c := range RecordFailureCounts() {
		if strings.HasPrefix(c.Database, prefix) {
			got = append(got, c.Database+"/"+c.Reason)
		}
	}
	want := []string{prefix + "_a/executor_disabled", prefix + "_a/policy_unavailable",
		prefix + "_b/no_typed_contract"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("counts = %v, want %v", got, want)
	}
}
