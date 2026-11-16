package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/leader"
)

// plan-02e: the plan engine claims steps as the fleet leader's holder,
// for the elector's lease, so a new leader resumes a step only after the
// old leader's claim expired. No concurrent access test: planClaim reads
// its arguments and the loaded config once.

func TestPlanClaim_IsTheElectorsHolderAndLease(t *testing.T) {
	e := leader.NewElector(stubLease{held: true}, "s", "host-a-41-0badf00d", 20*time.Second)
	owner, lease := planClaim(e)
	if owner != "host-a-41-0badf00d" || lease != 20*time.Second {
		t.Fatalf("planClaim = %q, %s; want the elector's holder and 20s", owner, lease)
	}
	long := leader.NewElector(stubLease{held: true}, "s", strings.Repeat("h", 300), 0)
	owner, lease = planClaim(long)
	if len(owner) != 100 || lease != leader.DefaultTTL {
		t.Fatalf("long holder: %d chars, %s; want 100 chars and the default TTL", len(owner),
			lease)
	}
}

func TestPlanClaim_BeforeTheElectorRunsUsesTheConfiguredLease(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = nil
	owner, lease := planClaim(nil)
	if !strings.HasSuffix(owner, fmt.Sprintf(":%d", os.Getpid())) ||
		lease != leader.DefaultTTL {
		t.Fatalf("no config: %q, %s; want this process and the default TTL", owner, lease)
	}
	cfg = &config.Config{}
	cfg.FleetLearning.LeaderLeaseSeconds = 45
	if _, lease = planClaim(nil); lease != 45*time.Second {
		t.Fatalf("configured lease: %s, want 45s", lease)
	}
	cfg.FleetLearning.LeaderLeaseSeconds = 0
	if _, lease = planClaim(leader.NewElector(stubLease{}, "s", "", 0)); lease !=
		leader.DefaultTTL {
		t.Fatalf("an elector without a holder: %s, want the default TTL", lease)
	}
}
