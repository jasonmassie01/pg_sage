package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// drill-02b fixture: the source is a database on the test server (PG14 in
// CHECK-35) holding a skewed column and its statistics; the "upgraded
// clone" is the same data on SAGE_TEST_SECOND_CLUSTER_URL (PG17) with no
// statistics, as pg_upgrade before PG18 leaves it. Without a second
// cluster the upgraded clone is a second database on the same server, so
// the rehearsal runs at one major (the statistics effect is the same).

const rehearsalDeployment = "0b5e2d4c-1f3a-4c55-9d1e-2a7b8c9d0e1f"

// The fixture's top statements.
const (
	qSkewed    = "SELECT count(*) FROM public.orders WHERE status = 'common'"
	qStable    = "SELECT note FROM public.orders WHERE id = 42"
	qSideEff   = "SELECT public.bump()"
	qForUpdate = "SELECT id FROM public.orders WHERE id = 1 FOR UPDATE"
	qParam     = "SELECT note FROM public.orders WHERE id = $1"
	qInsert    = "INSERT INTO public.calls VALUES (1)"
)

func fixtureTop() []TopQuery {
	return []TopQuery{{QueryID: 1, Text: qSkewed}, {QueryID: 2, Text: qStable},
		{QueryID: 3, Text: qSideEff}, {QueryID: 4, Text: qForUpdate},
		{QueryID: 5, Text: qParam}, {QueryID: 6, Text: qInsert}}
}

// seedSQL is the same data on both sides; autovacuum is off so the
// upgraded side keeps no statistics until the rehearsal's ANALYZE.
var seedSQL = []string{
	`CREATE TABLE public.orders (id bigint PRIMARY KEY, status text NOT NULL, note text)
	 WITH (autovacuum_enabled = false)`,
	`INSERT INTO public.orders SELECT g, CASE WHEN g % 100 = 0 THEN 'rare'
	 ELSE 'common' END, repeat('x', 40) FROM generate_series(1, 200000) g`,
	"CREATE INDEX orders_status ON public.orders (status)",
	"CREATE TABLE public.calls (n int)",
	`CREATE FUNCTION public.bump() RETURNS int LANGUAGE sql VOLATILE
	 AS 'INSERT INTO public.calls VALUES (1) RETURNING 1'`,
}

// fixtureShapes stand in for libpg_query where the build has no cgo, so
// the real analyze guard still proves each statement on the clone.
var fixtureShapes = map[string]sqlast.ReadQuery{
	qSkewed: {Functions: []sqlast.QualifiedName{{Name: "count"}},
		Operators: []sqlast.QualifiedName{{Name: "="}},
		Relations: []sqlast.QualifiedName{{Schema: "public", Name: "orders"}}},
	qStable: {Operators: []sqlast.QualifiedName{{Name: "="}},
		Relations: []sqlast.QualifiedName{{Schema: "public", Name: "orders"}}},
	qSideEff: {Functions: []sqlast.QualifiedName{{Schema: "public", Name: "bump"}}},
}

// fixtureInspect is nil (libpg_query) with cgo, else the shapes above.
func fixtureInspect() func(string) (sqlast.ReadQuery, error) {
	if _, err := sqlast.InspectReadQuery("SELECT 1"); !errors.Is(err, sqlast.ErrUnavailable) {
		return nil
	}
	return func(text string) (sqlast.ReadQuery, error) {
		if shape, ok := fixtureShapes[text]; ok {
			return shape, nil
		}
		return sqlast.ReadQuery{}, fmt.Errorf("fixture has no shape for %q", text)
	}
}

func notProductionServer(context.Context) (clone.ServerIdentity, error) {
	return clone.ServerIdentity{Host: "production.invalid", Port: 5432,
		StartedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ServerPort: 5432}, nil
}

// dsnOf is base pointed at database name.
func dsnOf(t *testing.T, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", testdb.EnvName, err)
	}
	u.Path = "/" + name
	return u.String()
}

// fixtureInner hands out the analyzed source copy as the clone.
type fixtureInner struct {
	dsn       string
	block     chan struct{} // non-nil: Create waits for it
	createErr error
	mu        sync.Mutex
	n         int
	destroyed []string
}

func (p *fixtureInner) Create(ctx context.Context, _ clone.CloneSpec) (clone.Clone, error) {
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return clone.Clone{}, ctx.Err()
		}
	}
	if p.createErr != nil {
		return clone.Clone{}, p.createErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return clone.Clone{DSN: p.dsn, ID: fmt.Sprintf("fixture-%d", p.n),
		CreatedFrom: time.Now()}, nil
}

func (p *fixtureInner) Destroy(_ context.Context, c clone.Clone) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, c.ID)
	return nil
}

func (p *fixtureInner) destroyedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.destroyed...)
}

func (p *fixtureInner) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

func (p *fixtureInner) Describe(context.Context) (clone.Capability, error) {
	return clone.Capability{Kind: clone.KindThinClone}, nil
}

// fixtureRehearser "upgrades" a clone by pointing it at the upgraded copy
// (the test fixture rehearser of the plan).
type fixtureRehearser struct {
	dsn     string
	err     error
	entered chan struct{} // non-nil: closed on entry, then wait for ctx
	mu      sync.Mutex
	calls   []string
}

func (f *fixtureRehearser) Upgrade(ctx context.Context, cl clone.Clone,
	major int) (clone.Clone, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s->%d", cl.ID, major))
	f.mu.Unlock()
	if f.entered != nil {
		close(f.entered)
		<-ctx.Done()
		return clone.Clone{}, ctx.Err()
	}
	if f.err != nil {
		return clone.Clone{}, f.err
	}
	return clone.Clone{DSN: f.dsn, ID: cl.ID, CreatedFrom: cl.CreatedFrom}, nil
}

// rehearsalFixture is the control database (receipts, briefs), the
// analyzed source copy and the upgraded copy without statistics.
type rehearsalFixture struct {
	control              liveDB
	sourceDSN, targetDSN string
	sourceMajor, target  int
	inner                *fixtureInner
	store                *clone.ReceiptStore
}

func newRehearsalFixture(t *testing.T) rehearsalFixture {
	t.Helper()
	control := bootstrapped(t)
	base := testdb.SkipUnlessLive(t)
	src := newDB(t, "")
	src.exec(t, seedSQL...)
	src.exec(t, "ANALYZE public.orders")
	f := rehearsalFixture{control: control, sourceDSN: dsnOf(t, base, src.name),
		sourceMajor: src.major, store: clone.NewReceiptStore(control.pool)}
	if os.Getenv(testdb.SecondClusterEnvName) != "" {
		f.targetDSN = testdb.CreateDatabaseOnSecondCluster(t, "upgreh")
		seedTarget(t, f.targetDSN)
	} else {
		dst := newDB(t, "")
		dst.exec(t, seedSQL...)
		f.targetDSN = dsnOf(t, base, dst.name)
	}
	f.target = serverMajor(t, f.targetDSN)
	f.inner = &fixtureInner{dsn: f.sourceDSN}
	return f
}

// receipting wraps the fixture provider in receipts written to control.
func (f rehearsalFixture) receipting(t *testing.T, lifetime context.Context,
	beat time.Duration) *clone.ReceiptingProvider {
	t.Helper()
	p, err := clone.NewReceiptingProvider(f.inner, f.store, clone.ReceiptingConfig{
		Adapter: "fixture", Scope: "drill-02b",
		Deployment: func(context.Context) (string, error) { return rehearsalDeployment, nil },
		Purpose:    clone.PurposeRehearsal, TTL: time.Hour, Lifetime: lifetime,
		HeartbeatEvery: beat, MaxCreatesPerHour: 60})
	if err != nil {
		t.Fatalf("receipting provider: %v", err)
	}
	return p
}

func (f rehearsalFixture) options(t *testing.T, p clone.Provider,
	up UpgradeRehearser) RehearsalOptions {
	t.Helper()
	return RehearsalOptions{Database: "app", SourceMajor: f.sourceMajor,
		TargetMajor: f.target, Provider: p, ProviderName: "fixture", Upgrader: up,
		Production: notProductionServer, StatementTimeout: 30 * time.Second,
		Top: func(context.Context, int) ([]TopQuery, error) {
			return fixtureTop(), nil
		}, Inspect: fixtureInspect()}
}
