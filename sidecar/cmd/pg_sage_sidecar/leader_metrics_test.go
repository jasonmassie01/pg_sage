package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/leader"
)

// fixedClock is a settable clock for elector metrics.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func tickedElector(t *testing.T, st leader.Store, scope string,
	clk *fixedClock) *leader.Elector {
	t.Helper()
	e := leader.NewElector(st, scope, "me", time.Minute, leader.WithClock(clk.now))
	_ = e.Tick(context.Background())
	return e
}

func TestWriteLeaderMetrics_LeaderAndFollowerScopes(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	meta := tickedElector(t, stubLease{held: true}, "meta", clk)
	gov := tickedElector(t, stubLease{held: false}, "agents:meta", clk)
	clk.t = clk.t.Add(5 * time.Second)
	var b strings.Builder
	writeLeaderMetrics(&b, meta, gov)
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_leader gauge\n",
		"# TYPE pg_sage_leader_transitions_total counter\n",
		"# TYPE pg_sage_leader_last_renew_age_seconds gauge\n",
		"pg_sage_leader{scope=\"meta\"} 1\n",
		"pg_sage_leader{scope=\"agents:meta\"} 0\n",
		"pg_sage_leader_transitions_total{scope=\"meta\"} 1\n",
		"pg_sage_leader_transitions_total{scope=\"agents:meta\"} 0\n",
		"pg_sage_leader_last_renew_age_seconds{scope=\"meta\"} 5.000\n",
		"pg_sage_leader_last_renew_age_seconds{scope=\"agents:meta\"} 5.000\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "# TYPE pg_sage_leader gauge"); n != 1 {
		t.Fatalf("TYPE header written %d times, want once", n)
	}
}

func TestWriteLeaderMetrics_NilAndDuplicateElectors(t *testing.T) {
	var b strings.Builder
	writeLeaderMetrics(&b, nil, nil)
	if b.Len() != 0 {
		t.Fatalf("election disabled must emit no leader series, got:\n%s", b.String())
	}
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	e := tickedElector(t, stubLease{held: true}, "meta", clk)
	b.Reset()
	writeLeaderMetrics(&b, e, e)
	if n := strings.Count(b.String(), "pg_sage_leader{scope=\"meta\"}"); n != 1 {
		t.Fatalf("a shared elector rendered %d times, want once:\n%s", n, b.String())
	}
}

type failingLease struct{}

func (failingLease) Acquire(context.Context, string, string,
	time.Duration) (leader.Lease, bool, error) {
	return leader.Lease{}, false, errors.New("connection refused")
}

func (failingLease) Release(context.Context, string, string) error { return nil }

func TestWriteLeaderMetrics_NeverRenewedAgesFromStart(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	e := tickedElector(t, failingLease{}, "meta", clk)
	clk.t = clk.t.Add(90 * time.Second)
	var b strings.Builder
	writeLeaderMetrics(&b, e)
	for _, want := range []string{
		"pg_sage_leader{scope=\"meta\"} 0\n",
		"pg_sage_leader_last_renew_age_seconds{scope=\"meta\"} 90.000\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics missing %q in:\n%s", want, b.String())
		}
	}
}

func TestAgentGovernanceLeader_RecordsDistinctElector(t *testing.T) {
	preserveAuditFenceGlobals(t)
	cfg = &config.Config{MetaDB: "postgres://meta"}
	cfg.FleetLearning.LeaderLeaseSeconds = 30
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := agentGovernanceLeader(ctx, lazyPool(t, "gov"), lazyPool(t, "fleet"))
	if got := governanceElector.Load(); got == nil || got != g.elector {
		t.Fatal("a separate governance elector must be kept for /metrics")
	}
	shared := lazyPool(t, "shared")
	_ = agentGovernanceLeader(ctx, shared, shared)
	if governanceElector.Load() != nil {
		t.Fatal("a shared control pool has no separate governance elector")
	}
}

func TestHandleMetrics_IncludesLeaderSeries(t *testing.T) {
	preserveAuditFenceGlobals(t)
	oldMgr, oldPool := fleetMgr, pool
	t.Cleanup(func() { fleetMgr, pool = oldMgr, oldPool })
	fleetMgr, pool = nil, nil
	cfg = &config.Config{Mode: "fleet", MetaDB: "postgres://meta"}
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	setFleetLeader(tickedElector(t, stubLease{held: true}, "meta", clk))
	rec := httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "pg_sage_leader{scope=\"meta\"} 1\n") {
		t.Fatalf("/metrics lacks the meta leader series:\n%s", rec.Body.String())
	}
}
