package rca

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Sage SRE M0: the 60 s lock-chain fast path opens or updates the
// lock_contention incident between analyzer cycles, records the root
// blocker's backend identity, and never changes the cycle-based
// escalation and auto-resolution semantics.

var fixedBackendStart = time.Date(2026, 9, 27, 8, 0, 0, 123456000, time.UTC)

func chainFinding(pid, blocked, depth int, state string) analyzer.Finding {
	sev := "warning"
	if blocked >= 10 {
		sev = "critical"
	}
	return analyzer.Finding{
		Category: "lock_chain", Severity: sev,
		ObjectIdentifier: fmt.Sprintf("pid:%d", pid),
		Detail: map[string]any{
			"pid":                pid,
			"root_blocker_pid":   pid,
			"backend_start":      fixedBackendStart.Add(time.Duration(pid) * time.Second),
			"query_start":        fixedBackendStart.Add(time.Minute),
			"query_id":           int64(9000 + pid),
			"query":              "UPDATE accounts SET secret = 'hunter2' WHERE id = 1",
			"root_blocker_state": state,
			"total_blocked":      blocked,
			"chain_depth":        depth,
			"locked_relation":    "public.accounts",
			"blocker_mode":       "RowExclusiveLock",
		},
	}
}

func lockIncidents(incs []Incident) []Incident {
	var out []Incident
	for _, inc := range incs {
		if stringsEqual(inc.SignalIDs, []string{"lock_contention"}) &&
			inc.ResolvedAt == nil {
			out = append(out, inc)
		}
	}
	return out
}

func onlyLockIncident(t *testing.T, eng *Engine) Incident {
	t.Helper()
	got := lockIncidents(eng.ActiveIncidents())
	if len(got) != 1 {
		t.Fatalf("open lock_contention incidents = %d, want 1: %+v", len(got), got)
	}
	return got[0]
}

func blockerLinks(inc Incident) []ChainLink {
	var out []ChainLink
	for _, l := range inc.CausalChain {
		if l.Blocker != nil {
			out = append(out, l)
		}
	}
	return out
}

func TestObserveLockChains_OpensIncidentWithBackendIdentity(t *testing.T) {
	eng := testEngine()
	eng.WithDatabaseName("orders_db")
	f := chainFinding(4242, 3, 2, "idle in transaction")

	got := eng.ObserveLockChains(context.Background(), []analyzer.Finding{f})
	if len(got) != 1 {
		t.Fatalf("ObserveLockChains returned %d incidents, want 1", len(got))
	}
	inc := onlyLockIncident(t, eng)
	if inc.DatabaseName != "orders_db" || inc.Source != "deterministic" ||
		inc.Severity != "warning" || inc.OccurrenceCount != 1 {
		t.Fatalf("incident = %+v", inc)
	}
	if !strings.Contains(inc.RootCause, "idle in transaction") ||
		!strings.Contains(inc.RootCause, "4242") {
		t.Errorf("root cause %q must name the idle-in-tx blocker pid", inc.RootCause)
	}
	links := blockerLinks(inc)
	if len(links) != 1 {
		t.Fatalf("blocker links = %d, want 1", len(links))
	}
	b := links[0].Blocker
	sum := sha256.Sum256([]byte(f.Detail["query"].(string)))
	wantSHA := hex.EncodeToString(sum[:8])
	if b.PID != 4242 || !b.BackendStart.Equal(f.Detail["backend_start"].(time.Time)) ||
		b.QueryID != 9000+4242 || b.QuerySHA != wantSHA ||
		b.State != "idle in transaction" || b.TotalBlocked != 3 ||
		b.ChainDepth != 2 || b.LockedRelation != "public.accounts" ||
		b.LockMode != "RowExclusiveLock" {
		t.Fatalf("blocker identity = %+v (want sha %s)", b, wantSHA)
	}
	for _, l := range inc.CausalChain {
		if strings.Contains(l.Evidence, "hunter2") ||
			strings.Contains(l.Description, "hunter2") {
			t.Fatalf("raw query text leaked into evidence: %+v", l)
		}
	}
	if !strings.Contains(links[0].Evidence, "backend_start=") ||
		!strings.Contains(links[0].Evidence, "pid=4242") {
		t.Errorf("evidence %q must carry pid and backend_start", links[0].Evidence)
	}
}

func TestObserveLockChains_NilAndEmptyAreNoOps(t *testing.T) {
	eng := testEngine()
	for name, in := range map[string][]analyzer.Finding{
		"nil":   nil,
		"empty": {},
		"other category": {{Category: "slow_query", Severity: "critical",
			Detail: map[string]any{"total_blocked": 9}}},
		"safe info with none blocked": {{Category: "lock_chain",
			Severity: "info", Detail: map[string]any{"total_blocked": 0}}},
	} {
		if got := eng.ObserveLockChains(context.Background(), in); got != nil {
			t.Errorf("%s: returned %d incidents, want nil", name, len(got))
		}
	}
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Fatalf("no-op observations created %d incidents", n)
	}
	var nilEng *Engine
	if got := nilEng.ObserveLockChains(context.Background(),
		[]analyzer.Finding{chainFinding(1, 3, 1, "active")}); got != nil {
		t.Fatal("nil engine must be a no-op")
	}
}

// Findings with malformed detail still fire the signal (the blocked count
// is real) but contribute no backend identity.
func TestObserveLockChains_MalformedDetailSkipsIdentity(t *testing.T) {
	eng := testEngine()
	bad := analyzer.Finding{Category: "lock_chain", Severity: "warning",
		Detail: map[string]any{"total_blocked": 4, "pid": "not-a-pid",
			"backend_start": "yesterday"}}
	eng.ObserveLockChains(context.Background(), []analyzer.Finding{bad})
	inc := onlyLockIncident(t, eng)
	if n := len(blockerLinks(inc)); n != 0 {
		t.Fatalf("blocker links = %d, want 0 for malformed detail", n)
	}
	if !strings.Contains(inc.CausalChain[0].Evidence, "4 total blocked") {
		t.Fatalf("summary evidence = %q", inc.CausalChain[0].Evidence)
	}
}

func TestObserveLockChains_RepeatUpdatesWithoutCountingCycles(t *testing.T) {
	eng := testEngine()
	ctx := context.Background()
	eng.ObserveLockChains(ctx, []analyzer.Finding{chainFinding(10, 3, 1, "active")})
	first := onlyLockIncident(t, eng)
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 10; i++ {
		eng.ObserveLockChains(ctx,
			[]analyzer.Finding{chainFinding(11, 4, 1, "idle in transaction")})
	}
	inc := onlyLockIncident(t, eng)
	if inc.ID != first.ID {
		t.Fatalf("fast path opened a second incident %s (first %s)", inc.ID, first.ID)
	}
	if inc.OccurrenceCount != 1 {
		t.Fatalf("OccurrenceCount = %d, want 1: fast ticks are not "+
			"analyzer cycles", inc.OccurrenceCount)
	}
	if inc.EscalatedAt != nil || inc.Severity != "warning" {
		t.Fatalf("fast ticks escalated the incident: %+v", inc)
	}
	if !inc.LastDetectedAt.After(first.LastDetectedAt) {
		t.Fatalf("LastDetectedAt did not advance: %s <= %s",
			inc.LastDetectedAt, first.LastDetectedAt)
	}
	links := blockerLinks(inc)
	if len(links) != 1 || links[0].Blocker.PID != 11 {
		t.Fatalf("chain not refreshed to the current blocker: %+v", links)
	}
	if !strings.Contains(inc.RootCause, "11") {
		t.Fatalf("root cause not refreshed: %q", inc.RootCause)
	}
	if eng.cycleCount != 0 {
		t.Fatalf("fast path advanced cycleCount to %d", eng.cycleCount)
	}
}

func TestObserveLockChains_SeverityOnlyRaises(t *testing.T) {
	eng := testEngine()
	ctx := context.Background()
	eng.ObserveLockChains(ctx, []analyzer.Finding{chainFinding(1, 12, 1, "active")})
	eng.ObserveLockChains(ctx, []analyzer.Finding{chainFinding(1, 3, 1, "active")})
	if inc := onlyLockIncident(t, eng); inc.Severity != "critical" {
		t.Fatalf("severity = %s, want critical kept", inc.Severity)
	}
}

// The slow analyzer cycle and the fast path converge on one incident.
func TestObserveLockChains_SlowCycleMergesSameIncident(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	ctx := context.Background()
	f := []analyzer.Finding{chainFinding(7, 3, 1, "active")}
	eng.ObserveLockChains(ctx, f)
	eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, f)
	inc := onlyLockIncident(t, eng)
	if inc.OccurrenceCount != 2 {
		t.Fatalf("OccurrenceCount = %d, want 2 (1 open + 1 analyzer cycle)",
			inc.OccurrenceCount)
	}
	if len(blockerLinks(inc)) != 1 {
		t.Fatalf("slow cycle lost the backend identity: %+v", inc.CausalChain)
	}
}

// A chain the fast path saw since the last analyzer cycle counts as
// "still firing" for auto-resolution, even if the analyzer's own probe
// happened to miss it.
func TestObserveLockChains_FastSightingDefersAutoResolve(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	ctx := context.Background()
	f := []analyzer.Finding{chainFinding(7, 3, 1, "active")}
	for i := 0; i < 4; i++ { // burn the grace period while firing
		eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, f)
	}
	eng.ObserveLockChains(ctx, f)
	eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, nil)
	eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, nil)
	if n := len(lockIncidents(eng.ActiveIncidents())); n != 1 {
		t.Fatal("incident resolved although the fast path saw the chain " +
			"within the resolution window")
	}
	eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, nil)
	if n := len(lockIncidents(eng.ActiveIncidents())); n != 0 {
		t.Fatal("incident must resolve after 2 clear cycles with no sighting")
	}
}

func TestObserveLockChains_DedupWindowBoundary(t *testing.T) {
	window := 30 * time.Minute
	for _, tc := range []struct {
		name      string
		age       time.Duration
		supersede bool
	}{
		{"inside window", window - time.Second, false},
		{"outside window", window + time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := testEngine()
			ctx := context.Background()
			f := []analyzer.Finding{chainFinding(3, 3, 1, "active")}
			eng.ObserveLockChains(ctx, f)
			old := onlyLockIncident(t, eng)
			eng.mu.Lock()
			eng.incidents[0].LastDetectedAt = time.Now().Add(-tc.age)
			eng.mu.Unlock()
			eng.ObserveLockChains(ctx, f)
			inc := onlyLockIncident(t, eng)
			if tc.supersede {
				if inc.ID == old.ID || inc.PreviousIncidentID != old.ID {
					t.Fatalf("stale incident not superseded/linked: %+v", inc)
				}
				return
			}
			if inc.ID != old.ID {
				t.Fatalf("incident inside the window was replaced")
			}
		})
	}
}

func TestLockEvidence_BlockerCapAndOrder(t *testing.T) {
	for _, n := range []int{maxEvidenceBlockers, maxEvidenceBlockers + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var fs []analyzer.Finding
			for i := 1; i <= n; i++ {
				fs = append(fs, chainFinding(100+i, i, 1, "active"))
			}
			bs := blockersFromFindings(fs)
			if len(bs) != maxEvidenceBlockers {
				t.Fatalf("blockers = %d, want cap %d", len(bs), maxEvidenceBlockers)
			}
			if bs[0].TotalBlocked != n || bs[0].PID != 100+n {
				t.Fatalf("first blocker = %+v, want the one blocking most", bs[0])
			}
			for i := 1; i < len(bs); i++ {
				if bs[i].TotalBlocked > bs[i-1].TotalBlocked {
					t.Fatalf("not sorted by blocked desc: %+v", bs)
				}
			}
		})
	}
}

// The chain query caps recursion at depth 10; deeper values are recorded
// as reported, never clamped or dropped.
func TestLockEvidence_ChainDepthRecorded(t *testing.T) {
	for _, depth := range []int{1, 9, 10} {
		bs := blockersFromFindings(
			[]analyzer.Finding{chainFinding(5, 3, depth, "active")})
		if len(bs) != 1 || bs[0].ChainDepth != depth {
			t.Fatalf("depth %d recorded as %+v", depth, bs)
		}
	}
}

// Tier 2 prompts are built from signal metrics; backend identities must
// not be added to them.
func TestLockEvidence_SignalMetricsUnchanged(t *testing.T) {
	eng := testEngine()
	sig := eng.detectLockContention(
		[]analyzer.Finding{chainFinding(5, 3, 1, "active")})
	if sig == nil {
		t.Fatal("signal did not fire")
	}
	if len(sig.Metrics) != 2 || sig.Metrics["total_blocked"] != 3 ||
		sig.Metrics["lock_chain_count"] != 1 {
		t.Fatalf("metrics = %v, want only lock_chain_count and total_blocked",
			sig.Metrics)
	}
}

// Concurrent access: two fast ticks and the analyzer racing on the same
// incident converge on exactly one open lock_contention incident.
func TestObserveLockChains_ConcurrentWithAnalyzer(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	ctx := context.Background()
	f := []analyzer.Finding{chainFinding(9, 3, 2, "idle in transaction")}
	eng.ObserveLockChains(ctx, f) // open first so the count is deterministic
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				eng.ObserveLockChains(ctx, f)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, f)
		}
	}()
	wg.Wait()
	inc := onlyLockIncident(t, eng)
	if inc.OccurrenceCount != 21 {
		t.Fatalf("OccurrenceCount = %d, want 21 (open + 20 analyzer cycles)",
			inc.OccurrenceCount)
	}
}
