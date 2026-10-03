package analyzer

import (
	"context"
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// A pg_sage session waiting for a lock (its catalog read queued behind an
// application's DDL) is not an application session blocked: the chain
// counts the application's waiters only. pg_sage holds lock_timeout on
// every statement, so its waits end on their own.
func TestLockChainsCountApplicationWaitersOnly(t *testing.T) {
	pool := phase2Pool(t)
	_, app, sage := selfload.Start(t, os.Getenv("SAGE_DATABASE_URL"))
	cfg := phase2Config()
	cfg.Analyzer.LockChain.Enabled = true
	chains, err := DetectLockChains(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("DetectLockChains: %v", err)
	}
	var root *LockChain
	for i := range chains {
		for _, pid := range chains[i].BlockedPIDs {
			if pid == sage.Waiter {
				t.Errorf("pg_sage waiter %d counted as blocked in chain of %d",
					pid, chains[i].RootBlockerPID)
			}
		}
		if chains[i].RootBlockerPID == app.Holder {
			root = &chains[i]
		}
	}
	if root == nil {
		t.Fatalf("no chain rooted at the application holder %d in %+v", app.Holder, chains)
	}
	if root.TotalBlocked != 1 || len(root.BlockedPIDs) != 1 ||
		root.BlockedPIDs[0] != app.Waiter {
		t.Fatalf("chain = %d blocked %v, want 1 blocked [%d] (the application waiter)",
			root.TotalBlocked, root.BlockedPIDs, app.Waiter)
	}
}
