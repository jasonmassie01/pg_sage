package analyzer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/lockqueue"
)

// migration-05: the lock-chain walk keeps its per-edge structure, so the
// queue behind a waiting DDL is visible as the DDL's direct waiters.
func TestDetectLockChains_DirectWaitersBehindDDL(t *testing.T) {
	pool := phase2Pool(t)
	s := lockqueue.Start(t, os.Getenv("SAGE_DATABASE_URL"), "public.lcw_queue", 3)
	cfg := phase2Config()
	cfg.Analyzer.LockChain.Enabled = true
	chains, err := DetectLockChains(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("DetectLockChains: %v", err)
	}
	var root *LockChain
	for i := range chains {
		if chains[i].RootBlockerPID == s.Root {
			root = &chains[i]
		}
	}
	if root == nil {
		t.Fatalf("no chain rooted at reader %d: %+v", s.Root, chains)
	}
	if root.DirectWaiters[s.DDL] != 3 {
		t.Fatalf("DirectWaiters[ddl %d] = %d, want 3 readers: %v",
			s.DDL, root.DirectWaiters[s.DDL], root.DirectWaiters)
	}
	if root.DirectWaiters[s.Root] != 1 {
		t.Fatalf("DirectWaiters[root %d] = %d, want 1 (the DDL): %v",
			s.Root, root.DirectWaiters[s.Root], root.DirectWaiters)
	}
	if root.TotalBlocked != 4 {
		t.Fatalf("TotalBlocked = %d, want 4", root.TotalBlocked)
	}
	if root.OldestWaitSince.IsZero() || time.Since(root.OldestWaitSince) > time.Minute {
		t.Fatalf("OldestWaitSince = %v, want the DDL's recent start", root.OldestWaitSince)
	}
}

// ProbeLockChains records every observation, an empty one included, and
// hands the queued-DDL source the chain so the root reader keeps no
// cancel SQL.
func TestProbeLockChains_RecordsAndRedirects(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Analyzer.LockChain = signalCfg()
	obs := NewLockChainObserver(time.Minute)
	if _, err := ProbeLockChains(context.Background(), pool, cfg, obs); err != nil {
		t.Fatalf("empty probe: %v", err)
	}
	if chains, _, ok := obs.Fresh(); !ok || len(chains) != 0 {
		t.Fatalf("empty probe observation = %v, %v; want fresh and empty", chains, ok)
	}
	s := lockqueue.Start(t, os.Getenv("SAGE_DATABASE_URL"), "public.lcw_probe", 3)
	obs.SetQueuedDDL(fakeQueued{s.DDL: true})
	findings, err := ProbeLockChains(context.Background(), pool, cfg, obs)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	chains, _, ok := obs.Fresh()
	if !ok || len(chains) == 0 {
		t.Fatalf("observation after the pile-up = %v, %v", chains, ok)
	}
	found := false
	for _, f := range findings {
		if f.Detail["pid"] != s.Root {
			continue
		}
		found = true
		if strings.Contains(f.RecommendedSQL, "pg_cancel_backend") ||
			f.Detail["queued_ddl_pid"] != s.DDL {
			t.Fatalf("root reader finding kept its cancel: sql=%q detail=%v",
				f.RecommendedSQL, f.Detail)
		}
	}
	if !found {
		t.Fatalf("no finding for root reader %d: %+v", s.Root, findings)
	}
}
