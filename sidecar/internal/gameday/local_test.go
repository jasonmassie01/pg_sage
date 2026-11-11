package gameday

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/clone"
)

// benches-02: the local provider's Create proves the target is on none of
// the refused clusters and is a marked, disposable primary; anything it
// cannot read refuses the run.

const scratchDSN = "postgres://u:p@scratch.internal:5999/scratch"

// refusing returns the refused side as a RefusedFunc.
func refusing(clusters ...*fakeCluster) RefusedFunc {
	return func(context.Context) ([]Querier, error) {
		out := make([]Querier, 0, len(clusters))
		for _, c := range clusters {
			out = append(out, c)
		}
		return out, nil
	}
}

// fakeLocal is a local provider whose target connection is target.
func fakeLocal(t *testing.T, dsn string, target *fakeCluster,
	refused RefusedFunc) (*LocalProvider, error) {
	t.Helper()
	p, err := NewLocalProvider(dsn, []string{"postgres://app:pw@127.0.0.1:5432/orders"},
		refused)
	if err != nil {
		return nil, err
	}
	p.connect = func(context.Context, string) (targetConn, error) { return target, nil }
	// The fake target has no server to take the bench target lock on.
	p.lock = grantLock
	return p, nil
}

func mustLocal(t *testing.T, dsn string, target *fakeCluster, refused RefusedFunc) *LocalProvider {
	t.Helper()
	p, err := fakeLocal(t, dsn, target, refused)
	if err != nil {
		t.Fatalf("local provider: %v", err)
	}
	return p
}

func TestLocalProviderAcceptsAMarkedDatabaseOnAnotherCluster(t *testing.T) {
	target, prod := scratchCluster(), prodCluster()
	p := mustLocal(t, scratchDSN, target, refusing(prod))
	c, err := p.Create(context.Background(), clone.CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatalf("a marked database on another cluster was refused: %v", err)
	}
	if c.DSN != scratchDSN || c.ID != "local" || c.CreatedFrom.IsZero() {
		t.Fatalf("local clone = %+v", c)
	}
	if target.closed != 1 || prod.queries == 0 {
		t.Fatalf("target closed %d times, refused side read %d times", target.closed,
			prod.queries)
	}
}

// Aliases of the monitored cluster (another host spelling, a pooler
// port, a sibling database) all reach one identity. A multi-host DSN is
// refused at boot (TestLocalProviderRefusesAMultiServerTarget).
func TestLocalProviderRefusesAliasesOfTheMonitoredCluster(t *testing.T) {
	for _, dsn := range []string{
		"postgres://app:pw@localhost:5432/orders",
		"postgres://app:pw@LOCALHOST:5432/orders",
		"postgres://app:pw@127.0.0.1:6432/orders",
		"postgres://app:pw@127.0.0.1:5432/scratch",
	} {
		prod := prodCluster()
		target := prodCluster()
		target.marker = marked(TargetMarker) // the marker never overrides identity
		p := mustLocal(t, dsn, target, refusing(scratchCluster(), prod))
		_, err := p.Create(context.Background(), clone.CloneSpec{})
		if !errors.Is(err, ErrMonitoredDSN) {
			t.Errorf("%s: err = %v, want ErrMonitoredDSN", dsn, err)
		}
		if err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the refusal leaks the password: %v", dsn, err)
		}
		if target.closed != 1 {
			t.Errorf("%s: target connection closed %d times", dsn, target.closed)
		}
	}
}

// Create verifies the one server it reaches; a DSN that can fail over to
// another server could run the fault programs where nothing was checked.
func TestLocalProviderRefusesAMultiServerTarget(t *testing.T) {
	for dsn, other := range map[string]string{
		"postgres://app:pw@db1.internal:5432,127.0.0.1:5432/orders":     "127.0.0.1:5432",
		"host=scratch.internal,prod.internal port=5999 dbname=scratch":  "prod.internal:5999",
		"postgres://u:pw@scratch.internal:5999,scratch.internal:5432/s": "scratch.internal:5432",
	} {
		p, err := NewLocalProvider(dsn, nil, refusing(prodCluster()))
		if !errors.Is(err, ErrNotDisposable) || p != nil {
			t.Errorf("%s: built %t, err = %v, want ErrNotDisposable", dsn, p != nil, err)
			continue
		}
		if !strings.Contains(err.Error(), other) || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the refusal should name %s and no password: %v", dsn, other, err)
		}
	}
	// sslmode=prefer adds a fallback to the same server; another spelling
	// of one host name is the same server too.
	for _, dsn := range []string{scratchDSN + "?sslmode=prefer",
		"postgres://u:p@SCRATCH.internal:5999,scratch.internal:5999/scratch"} {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil || len(cfg.Fallbacks) == 0 {
			t.Fatalf("%s: want a parsed DSN with fallbacks: %v", dsn, err)
		}
		if _, err := fakeLocal(t, dsn, scratchCluster(), refusing(prodCluster())); err != nil {
			t.Errorf("%s: a single server was refused: %v", dsn, err)
		}
	}
}

func TestLocalProviderRefusesTargetsThatAreNotDisposable(t *testing.T) {
	cases := map[string]func(c *fakeCluster){
		"in recovery":   func(c *fakeCluster) { c.id.InRecovery = true },
		"has standbys":  func(c *fakeCluster) { c.replicas = 1 },
		"no marker":     func(c *fakeCluster) { c.marker = nil },
		"another label": func(c *fakeCluster) { c.marker = marked("scratch database") },
		"marker prefix": func(c *fakeCluster) { c.marker = marked(TargetMarker + " (old)") },
	}
	for name, mutate := range cases {
		target := scratchCluster()
		mutate(target)
		p := mustLocal(t, scratchDSN, target, refusing(prodCluster()))
		_, err := p.Create(context.Background(), clone.CloneSpec{})
		if !errors.Is(err, ErrNotDisposable) {
			t.Errorf("%s: err = %v, want ErrNotDisposable", name, err)
			continue
		}
		if strings.HasPrefix(name, "no marker") &&
			!strings.Contains(err.Error(), "COMMENT ON DATABASE") {
			t.Errorf("%s: the refusal does not name the fix: %v", name, err)
		}
	}
}

func TestLocalProviderNilAndEmptyRefusedSide(t *testing.T) {
	if p, err := NewLocalProvider(scratchDSN, nil, nil); err == nil || p != nil {
		t.Fatalf("a nil refused func built a provider: %v %v", p, err)
	}
	if _, err := NewLocalProvider("", nil, refusing()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty dsn: err = %v, want ErrNotConfigured", err)
	}
	// Nothing to compare against: the sidecar always has a metadata
	// database, so an empty refused side is a wiring fault and refuses.
	p := mustLocal(t, scratchDSN, scratchCluster(), refusing())
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		ErrNotDisposable) {
		t.Fatalf("empty refused side: err = %v, want ErrNotDisposable", err)
	}
	cause := errors.New("fleet manager unavailable")
	p = mustLocal(t, scratchDSN, scratchCluster(),
		func(context.Context) ([]Querier, error) { return nil, cause })
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err, cause) {
		t.Fatalf("refused func error: err = %v, want it wrapped", err)
	}
	p = mustLocal(t, scratchDSN, scratchCluster(),
		func(context.Context) ([]Querier, error) { return []Querier{nil}, nil })
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err == nil {
		t.Fatal("a nil refused querier was treated as a different cluster")
	}
}

func TestLocalProviderFailsClosedOnUnreadableIdentities(t *testing.T) {
	cause := errors.New("permission denied for function pg_postmaster_start_time")
	// The target's two identity probes both fail.
	target := scratchCluster()
	target.sysErr, target.identErr = errPermission, cause
	p := mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	_, err := p.Create(context.Background(), clone.CloneSpec{})
	if !errors.Is(err, ErrNotDisposable) || !errors.Is(err, cause) {
		t.Fatalf("unreadable target: err = %v, want ErrNotDisposable wrapping the cause", err)
	}
	// CHECK-06: a refused side with no privilege on either probe.
	prod := prodCluster()
	prod.sysErr, prod.identErr = errPermission, cause
	p = mustLocal(t, scratchDSN, scratchCluster(), refusing(prod))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		ErrNotDisposable) || !errors.Is(err, cause) {
		t.Fatalf("unreadable refused side: err = %v, want a refusal wrapping the cause", err)
	}
	// The target's marker and standby facts are unreadable.
	target = scratchCluster()
	target.factsErr = cause
	p = mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err, cause) {
		t.Fatalf("unreadable target facts: err = %v", err)
	}
	// The target cannot be reached.
	p = mustLocal(t, scratchDSN, scratchCluster(), refusing(prodCluster()))
	p.connect = func(context.Context, string) (targetConn, error) { return nil, cause }
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		ErrNotDisposable) || !errors.Is(err, cause) {
		t.Fatalf("unreachable target: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = mustLocal(t, scratchDSN, scratchCluster(), refusing(prodCluster()))
	if _, err := p.Create(ctx, clone.CloneSpec{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: err = %v, want context.Canceled", err)
	}
}

// Without pg_control_system the catalog hash and start time decide.
func TestLocalProviderIdentityFallbackBoundaries(t *testing.T) {
	// The target's system id is unreadable; its catalog and start time
	// match the monitored cluster: the same cluster.
	target := prodCluster()
	target.sysErr, target.marker = errPermission, marked(TargetMarker)
	p := mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		ErrMonitoredDSN) {
		t.Fatalf("unknown system id, equal catalog and start: err = %v", err)
	}
	// Equal catalog (two fresh clusters), different start: another cluster.
	target = scratchCluster()
	target.sysErr, target.id.CatalogHash = errPermission, "hash-prod"
	p = mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err != nil {
		t.Fatalf("equal catalog with a different start was refused: %v", err)
	}
	// Equal system ids decide even when the catalog differs (a standby
	// that has not replayed a CREATE DATABASE yet).
	target = scratchCluster()
	target.id.SystemID = prodCluster().id.SystemID
	p = mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		ErrMonitoredDSN) {
		t.Fatalf("equal system ids: err = %v, want ErrMonitoredDSN", err)
	}
}

// Every refused cluster is checked, not only the first.
func TestLocalProviderChecksEveryRefusedCluster(t *testing.T) {
	target := prodCluster()
	target.marker = marked(TargetMarker)
	other := scratchCluster()
	other.marker = nil
	p := mustLocal(t, scratchDSN, target, refusing(other, prodCluster()))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		ErrMonitoredDSN) {
		t.Fatalf("the second refused cluster was not compared: err = %v", err)
	}
}

// Runner and LocalBench share one provider; Create keeps no state
// between calls, so parallel calls each verify their own connection.
func TestLocalProviderCreateIsSafeConcurrently(t *testing.T) {
	var connects atomic.Int64
	targets := make(chan *fakeCluster, 64)
	p := mustLocal(t, scratchDSN, nil, refusing(prodCluster()))
	p.connect = func(context.Context, string) (targetConn, error) {
		connects.Add(1)
		c := scratchCluster()
		targets <- c
		return c, nil
	}
	b, err := NewLocalBench(LocalBenchConfig{Database: "orders", Provider: "local"}, p,
		&fakeFaults{report: []byte(scoredReport)}, &fakeBenchLedger{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := p.Create(context.Background(), clone.CloneSpec{}); err != nil ||
				c.DSN != scratchDSN {
				errs <- errors.Join(err, errors.New("wrong clone "+c.ID))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if run, err := b.Run(context.Background(), nil); err != nil ||
			run.Status != StatusCompleted {
			errs <- errors.Join(err, errors.New("local bench "+run.Status))
		}
	}()
	wg.Wait()
	close(errs)
	close(targets)
	for err := range errs {
		t.Error(err)
	}
	if connects.Load() != 17 {
		t.Fatalf("connects = %d, want one per Create (17)", connects.Load())
	}
	for c := range targets {
		if c.closed != 1 {
			t.Fatalf("a target connection was closed %d times", c.closed)
		}
	}
}

// A refused local target fails the bench run before any fault program.
func TestLocalBenchRefusedTargetFailsTheRun(t *testing.T) {
	target := prodCluster()
	target.marker = marked(TargetMarker)
	p := mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	f, l := &fakeFaults{report: []byte(scoredReport)}, &fakeBenchLedger{}
	b, err := NewLocalBench(LocalBenchConfig{Database: "orders", Provider: "local"}, p, f, l)
	if err != nil {
		t.Fatal(err)
	}
	run, err := b.Run(context.Background(), []string{"lock_blocking"})
	if !errors.Is(err, ErrMonitoredDSN) || run.Status != StatusFailed ||
		!strings.Contains(run.Error, "monitored") || run.FinishedAt == nil {
		t.Fatalf("refused run = %+v (%v)", run, err)
	}
	if len(f.dsns) != 0 || len(l.calls) != 0 {
		t.Fatalf("fault programs ran %d times, ledger called %d times", len(f.dsns),
			len(l.calls))
	}
	if b.Running() {
		t.Fatal("the run slot was not released")
	}
}

// A refused local target fails the game day and records why (live store).
func TestGameDayRefusedLocalTargetIsStoredAsFailed(t *testing.T) {
	f := newGDFixture(t)
	target := scratchCluster()
	target.marker = nil
	p := mustLocal(t, scratchDSN, target, refusing(prodCluster()))
	r, err := NewRunner(Config{Database: f.database, Provider: "local",
		Now: func() time.Time { return f.now }}, p, f.faults, f.ledger, f.store)
	if err != nil {
		t.Fatal(err)
	}
	gd, err := r.Run(f.ctx)
	if !errors.Is(err, ErrNotDisposable) || gd.Status != StatusFailed {
		t.Fatalf("game day = %+v (%v)", gd, err)
	}
	if len(f.faults.dsns) != 0 {
		t.Fatalf("fault programs ran on a refused target: %v", f.faults.dsns)
	}
	list, err := r.List(f.ctx, 1)
	if err != nil || len(list) != 1 || list[0].Status != StatusFailed ||
		!strings.Contains(list[0].Error, "COMMENT ON DATABASE") {
		t.Fatalf("stored game day = %+v (%v)", list, err)
	}
}
