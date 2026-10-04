package runway

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The monitor's wraparound_tables read is background sampling: it runs
// with the probe's background budget. On PostgreSQL 14 its per-table
// statistics come from the stats collector's file, and a backend waits
// for a fresh one inside its statement (measured on an idle PG14 under
// host load: 10 ms typically, 2.66 s once), so the 500 ms investigation
// budget failed the sample (PGIncidentBench seq-cycling-near-limit, run
// 37169374833: "probe wraparound_tables error: statement_timeout").

// methodRunner records which entry point ran each probe.
type methodRunner struct {
	inner ProbeRunner
	mu    sync.Mutex
	calls map[probes.ID][]string
}

func (m *methodRunner) note(id probes.ID, how string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[probes.ID][]string{}
	}
	m.calls[id] = append(m.calls[id], how)
}

func (m *methodRunner) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	m.note(id, "run")
	return m.inner.Run(ctx, id, a)
}

func (m *methodRunner) RunBackground(ctx context.Context, id probes.ID,
	a probes.Args) probes.Result {
	m.note(id, "background")
	return m.inner.RunBackground(ctx, id, a)
}

func TestMonitorSample_WraparoundTablesUsesBackgroundBudget(t *testing.T) {
	pool, ctx := livePool(t)
	rec := &methodRunner{inner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))}
	m := newTestMonitor(t, pool, rec, nil)
	if _, err := m.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	got := rec.calls[probes.WraparoundTablesProbe]
	if len(got) != 1 || got[0] != "background" {
		t.Fatalf("wraparound_tables ran via %v, want one background run", got)
	}
	// The investigation-shaped reads keep the foreground budget.
	if x := rec.calls[probes.XIDRunwayProbe]; len(x) != 1 || x[0] != "run" {
		t.Fatalf("xid_runway ran via %v, want one foreground run", x)
	}
}

func TestWraparoundTablesSpec_HasBackgroundBudget(t *testing.T) {
	s, ok := probes.Catalog().Spec(probes.WraparoundTablesProbe)
	if !ok {
		t.Fatal("wraparound_tables is not in the catalog")
	}
	if s.BackgroundTimeout != probes.MaxBackgroundStatementTimeout {
		t.Fatalf("background timeout = %s, want %s", s.BackgroundTimeout,
			probes.MaxBackgroundStatementTimeout)
	}
	if s.StatementTimeout != probes.MaxStatementTimeout {
		t.Fatalf("investigation budget = %s, want it unchanged at %s", s.StatementTimeout,
			probes.MaxStatementTimeout)
	}
}
