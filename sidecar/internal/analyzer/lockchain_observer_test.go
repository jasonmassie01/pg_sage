package analyzer

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// migration-05: the lock-chain probe keeps its last observation per
// database so the migration detector can see the queue behind a DDL.

func fixedObserver(interval time.Duration, now *time.Time) *LockChainObserver {
	o := NewLockChainObserver(interval)
	o.now = func() time.Time { return *now }
	return o
}

// Boundary: an observation is fresh for exactly two fast-path intervals.
func TestLockChainObserver_FreshForTwoIntervals(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := t0
	o := fixedObserver(60*time.Second, &now)
	o.Record([]LockChain{{RootBlockerPID: 7, BlockedPIDs: []int{8}}})
	now = t0.Add(120 * time.Second)
	chains, at, ok := o.Fresh()
	if !ok || len(chains) != 1 || chains[0].RootBlockerPID != 7 || !at.Equal(t0) {
		t.Fatalf("at 2 intervals Fresh() = %v, %v, %v; want the chain seen at %v",
			chains, at, ok, t0)
	}
	now = t0.Add(121 * time.Second)
	if chains, _, ok := o.Fresh(); ok || chains != nil {
		t.Fatalf("past 2 intervals Fresh() = %v, %v; want stale", chains, ok)
	}
}

// Nil/empty: no observation yet, a disabled fast path (interval 0) and a
// nil observer are never fresh.
func TestLockChainObserver_NotFreshWithoutObservation(t *testing.T) {
	now := time.Now()
	if _, _, ok := fixedObserver(time.Minute, &now).Fresh(); ok {
		t.Fatal("an observer that never recorded reported a fresh observation")
	}
	off := fixedObserver(0, &now)
	off.Record([]LockChain{{RootBlockerPID: 1}})
	if _, _, ok := off.Fresh(); ok {
		t.Fatal("interval 0 (fast path disabled) reported a fresh observation")
	}
	var nilObs *LockChainObserver
	nilObs.Record([]LockChain{{RootBlockerPID: 1}})
	nilObs.Nudge()
	nilObs.SetQueuedDDL(nil)
	if _, _, ok := nilObs.Fresh(); ok {
		t.Fatal("nil observer reported a fresh observation")
	}
}

// An empty observation is still an observation: "no queue" is known.
func TestLockChainObserver_EmptyObservationIsFresh(t *testing.T) {
	now := time.Now()
	o := fixedObserver(time.Minute, &now)
	o.Record(nil)
	chains, _, ok := o.Fresh()
	if !ok || len(chains) != 0 {
		t.Fatalf("Fresh() after an empty observation = %v, %v; want fresh, no chains",
			chains, ok)
	}
}

func TestLockChainObserver_NudgeCallsNudger(t *testing.T) {
	o := NewLockChainObserver(time.Minute)
	o.Nudge() // no nudger: nothing happens
	calls := 0
	o.SetNudger(func() { calls++ })
	o.Nudge()
	o.Nudge()
	if calls != 2 {
		t.Fatalf("nudger called %d times, want 2 (the ticker rate-limits)", calls)
	}
}

// Fresh returns a copy: a caller editing it cannot change what the next
// reader sees.
func TestLockChainObserver_FreshReturnsCopy(t *testing.T) {
	now := time.Now()
	o := fixedObserver(time.Minute, &now)
	o.Record([]LockChain{{RootBlockerPID: 3, BlockedPIDs: []int{4},
		DirectWaiters: map[int]int{3: 1}}})
	got, _, _ := o.Fresh()
	got[0].RootBlockerPID = 99
	got[0].BlockedPIDs[0] = 99
	got[0].DirectWaiters[3] = 99
	again, _, _ := o.Fresh()
	if again[0].RootBlockerPID != 3 || again[0].BlockedPIDs[0] != 4 ||
		again[0].DirectWaiters[3] != 1 {
		t.Fatalf("observation changed through a returned copy: %+v", again[0])
	}
}

// Concurrent access: the fast path and the analyzer cycle record while
// the migration detector reads.
func TestLockChainObserver_ConcurrentRecordAndRead(t *testing.T) {
	o := NewLockChainObserver(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(pid int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				o.Record([]LockChain{{RootBlockerPID: pid,
					DirectWaiters: map[int]int{pid: j}}})
			}
		}(i + 1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if chains, _, ok := o.Fresh(); ok && len(chains) != 1 {
					t.Errorf("torn observation: %v", chains)
				}
			}
		}()
	}
	wg.Wait()
	if chains, _, ok := o.Fresh(); !ok || len(chains) != 1 {
		t.Fatalf("final observation = %v, %v", chains, ok)
	}
}

func TestDirectWaitersFromEdges(t *testing.T) {
	// A (root) <- B (DDL) <- C, D, E (readers).
	edges := [][]int32{{200, 100}, {300, 200}, {301, 200}, {302, 200}, {300, 200}}
	got := directWaiters(edges)
	if len(got) != 2 || got[100] != 1 || got[200] != 3 {
		t.Fatalf("directWaiters = %v, want {100:1 200:3}", got)
	}
	if got := directWaiters(nil); got == nil || len(got) != 0 {
		t.Fatalf("directWaiters(nil) = %v, want an empty map", got)
	}
	// Malformed edges (wrong arity) are ignored, not counted.
	if got := directWaiters([][]int32{{1}, {1, 2, 3}, {5, 6}}); len(got) != 1 ||
		got[6] != 1 {
		t.Fatalf("directWaiters(malformed) = %v, want {6:1}", got)
	}
}

type fakeQueued map[int]bool

func (f fakeQueued) QueuedDDLPID(c LockChain) (int, bool) {
	for _, pid := range c.BlockedPIDs {
		if f[pid] {
			return pid, true
		}
	}
	return 0, false
}

func queuedChain(state string) LockChain {
	return LockChain{
		RootBlockerPID: 100, RootBlockerState: state,
		RootBlockerQuery: "SELECT pg_sleep(60) FROM t", RootBlockerApp: "app",
		RootBlockerSince:        time.Now().Add(-time.Hour),
		RootBlockerBackendStart: time.Now().Add(-2 * time.Hour),
		ChainDepth:              2, BlockedPIDs: []int{200, 300, 301, 302},
		TotalBlocked: 4, DirectWaiters: map[int]int{100: 1, 200: 3},
	}
}

func signalCfg() config.LockChainConfig {
	return config.LockChainConfig{Enabled: true, MinBlockedThreshold: 1,
		CriticalBlockedThreshold: 3, ActiveQueryCancelMinutes: 0,
		IdleInTxTerminateMinutes: 0}
}

// The root reader's cancel is the wrong remedy when the queue is behind a
// waiting DDL: the finding keeps its evidence, drops the signal SQL and
// points to the migration finding.
func TestRedirectQueuedDDL_DropsRootSignal(t *testing.T) {
	for _, state := range []string{"active", "idle in transaction"} {
		chains := []LockChain{queuedChain(state)}
		findings := lockChainFindings(chains, signalCfg(), 1)
		if len(findings) != 1 || findings[0].RecommendedSQL == "" {
			t.Fatalf("%s: precondition: want one finding with signal SQL, got %+v",
				state, findings)
		}
		redirectQueuedDDL(findings, chains, fakeQueued{200: true})
		f := findings[0]
		if f.RecommendedSQL != "" || f.ActionRisk != "" {
			t.Fatalf("%s: signal kept: sql=%q risk=%q", state, f.RecommendedSQL,
				f.ActionRisk)
		}
		if f.Detail["queued_ddl_pid"] != 200 {
			t.Fatalf("%s: queued_ddl_pid = %v, want 200", state, f.Detail["queued_ddl_pid"])
		}
		if !strings.Contains(f.Recommendation, "migration_safety") ||
			!strings.Contains(f.Recommendation, "200") {
			t.Fatalf("%s: recommendation %q does not point to the DDL's "+
				"migration finding", state, f.Recommendation)
		}
		if f.Detail["pid"] != 100 || f.Detail["total_blocked"] != 4 {
			t.Fatalf("%s: evidence lost: %v", state, f.Detail)
		}
		if f.Severity != "critical" {
			t.Fatalf("%s: severity = %q, want the original critical", state, f.Severity)
		}
	}
}

// Nil/empty: no queued-DDL source, or a chain without a tracked DDL,
// leaves the finding exactly as built.
func TestRedirectQueuedDDL_NoQueuedDDLKeepsFinding(t *testing.T) {
	for name, q := range map[string]QueuedDDL{
		"nil": nil, "none": fakeQueued{}, "other pid": fakeQueued{999: true},
	} {
		chains := []LockChain{queuedChain("active")}
		findings := lockChainFindings(chains, signalCfg(), 1)
		want := findings[0].RecommendedSQL
		redirectQueuedDDL(findings, chains, q)
		if findings[0].RecommendedSQL != want || want == "" {
			t.Fatalf("%s: RecommendedSQL = %q, want unchanged %q", name,
				findings[0].RecommendedSQL, want)
		}
		if _, ok := findings[0].Detail["queued_ddl_pid"]; ok {
			t.Fatalf("%s: queued_ddl_pid set without a queued DDL", name)
		}
	}
	redirectQueuedDDL(nil, nil, fakeQueued{200: true}) // no findings: no panic
}
