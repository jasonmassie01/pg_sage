package migration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func quietLog(string, string, ...any) {}

// fakeRepack is a probe answer with a call counter.
type fakeRepack struct {
	calls     atomic.Int32
	available bool
	err       error
}

func (f *fakeRepack) probe(context.Context) (bool, error) {
	f.calls.Add(1)
	return f.available, f.err
}

func assessorWithRepack(f *fakeRepack) *RiskAssessor {
	ra := NewRiskAssessor(nil, quietLog)
	ra.repack = newRepackCache(f.probe)
	return ra
}

func classifyOne(t *testing.T, sql, rule string) DDLClassification {
	t.Helper()
	for _, c := range NewRegexClassifier().Classify(sql, 0) {
		if c.RuleID == rule {
			return c
		}
	}
	t.Fatalf("%q did not classify as %s", sql, rule)
	return DDLClassification{}
}

func assessAlternative(t *testing.T, ra *RiskAssessor, c DDLClassification) string {
	t.Helper()
	risk, err := ra.Assess(context.Background(), c)
	if err != nil || risk == nil {
		t.Fatalf("Assess(%s): risk=%v err=%v", c.RuleID, risk, err)
	}
	return risk.SafeAlternative
}

// Happy path (migration-12): without pg_repack on the server the VACUUM
// FULL and CLUSTER alternatives do not recommend it.
func TestRepackAlternativeOmittedWhenUnavailable(t *testing.T) {
	f := &fakeRepack{available: false}
	ra := assessorWithRepack(f)
	vac := assessAlternative(t, ra, classifyOne(t, "VACUUM FULL orders", "ddl_vacuum_full"))
	clu := assessAlternative(t, ra, classifyOne(t, "CLUSTER orders", "ddl_cluster"))
	if strings.Contains(vac, "pg_repack") || !strings.Contains(vac, "VACUUM") {
		t.Errorf("VACUUM FULL alternative = %q, want regular VACUUM without pg_repack", vac)
	}
	if strings.Contains(clu, "pg_repack") || !strings.Contains(clu, "maintenance window") {
		t.Errorf("CLUSTER alternative = %q, want a maintenance window without pg_repack", clu)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("probe ran %d times for two assessments, want 1 (cached)", got)
	}
}

// With pg_repack available both alternatives mention it.
func TestRepackAlternativeNamedWhenAvailable(t *testing.T) {
	ra := assessorWithRepack(&fakeRepack{available: true})
	vac := assessAlternative(t, ra, classifyOne(t, "VACUUM FULL orders", "ddl_vacuum_full"))
	clu := assessAlternative(t, ra, classifyOne(t, "CLUSTER orders", "ddl_cluster"))
	if !strings.Contains(vac, "pg_repack") || !strings.Contains(clu, "pg_repack") {
		t.Fatalf("alternatives = %q / %q, want both to name pg_repack", vac, clu)
	}
}

// Error propagation: a failed probe omits pg_repack (never recommend a
// tool we could not confirm) and is retried on the next assessment; the
// first success is cached.
func TestRepackProbeFailureOmitsAndRetries(t *testing.T) {
	f := &fakeRepack{err: errors.New("permission denied for pg_available_extensions")}
	ra := assessorWithRepack(f)
	c := classifyOne(t, "VACUUM FULL orders", "ddl_vacuum_full")
	if alt := assessAlternative(t, ra, c); strings.Contains(alt, "pg_repack") {
		t.Fatalf("failed probe alternative = %q, want no pg_repack", alt)
	}
	f.err, f.available = nil, true
	if alt := assessAlternative(t, ra, c); !strings.Contains(alt, "pg_repack") {
		t.Fatalf("after recovery alternative = %q, want pg_repack", alt)
	}
	assessAlternative(t, ra, c)
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("probe ran %d times, want 2 (one failure, one cached success)", got)
	}
}

// Rules that never mention pg_repack do not run the probe.
func TestRepackProbeSkippedForOtherRules(t *testing.T) {
	f := &fakeRepack{available: true}
	ra := assessorWithRepack(f)
	c := classifyOne(t, "CREATE INDEX i ON orders (a)", "ddl_index_not_concurrent")
	if alt := assessAlternative(t, ra, c); alt != c.SafeAlternative {
		t.Fatalf("alternative = %q, want the rule's own %q", alt, c.SafeAlternative)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("probe ran %d times for CREATE INDEX, want 0", got)
	}
}

// Nil/empty: without a pool (no server to ask) pg_repack is not named,
// and the rule templates themselves never name it.
func TestRepackOmittedWithoutPoolAndInTemplates(t *testing.T) {
	ra := NewRiskAssessor(nil, quietLog)
	c := classifyOne(t, "VACUUM FULL orders", "ddl_vacuum_full")
	if alt := assessAlternative(t, ra, c); strings.Contains(alt, "pg_repack") {
		t.Fatalf("pool-less alternative = %q, want no pg_repack", alt)
	}
	for _, r := range ruleCatalog() {
		if strings.Contains(r.SafeAltTemplate, "pg_repack") {
			t.Errorf("rule %s template %q names pg_repack unconditionally", r.ID,
				r.SafeAltTemplate)
		}
	}
}

// Concurrent access: the activity poller and the log detector share one
// advisor; parallel assessments probe once and agree.
func TestRepackProbeRunsOnceUnderConcurrency(t *testing.T) {
	f := &fakeRepack{available: true}
	ra := assessorWithRepack(f)
	c := classifyOne(t, "VACUUM FULL orders", "ddl_vacuum_full")
	var wg sync.WaitGroup
	alts := make([]string, 16)
	for i := range alts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			risk, err := ra.Assess(context.Background(), c)
			if err == nil {
				alts[i] = risk.SafeAlternative
			}
		}(i)
	}
	wg.Wait()
	for i, alt := range alts {
		if !strings.Contains(alt, "pg_repack") {
			t.Errorf("assessment %d alternative = %q, want pg_repack", i, alt)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("probe ran %d times across 16 parallel assessments, want 1", got)
	}
}

// Integration: on a real server the alternative names pg_repack exactly
// when pg_available_extensions lists it.
func TestIntegration_RepackAlternativeFollowsServer(t *testing.T) {
	pool, ctx := requireDB(t)
	var available bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_available_extensions
		WHERE name = 'pg_repack')`).Scan(&available); err != nil {
		t.Fatalf("read pg_available_extensions: %v", err)
	}
	c := classifyOne(t, "VACUUM FULL orders", "ddl_vacuum_full")
	risk, err := NewRiskAssessor(pool, quietLog).Assess(ctx, c)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if got := strings.Contains(risk.SafeAlternative, "pg_repack"); got != available {
		t.Fatalf("alternative %q names pg_repack=%v, server offers it=%v",
			risk.SafeAlternative, got, available)
	}
}
