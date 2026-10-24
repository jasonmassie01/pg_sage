package receipt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Fixtures for the receipts: one disposable database per package process
// with the sage schema bootstrapped once; every test starts from empty
// review, history, incident, finding and change-feed tables. Reviews are
// seeded through the foundation's Store.Insert; nothing here calls an LLM
// or runs the reviewed change against anything but a test database.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/changereview/receipt"))
}

var (
	bootstrapOnce sync.Once
	bootstrapErr  error
)

// clock is an adjustable clock: real time plus an offset.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	name   string
	clock  *clock
	cfg    config.ChangeReviewConfig
	svc    *changereview.Service
	events *changefeed.Store
	feed   *changefeed.Feed
	scope  sre.Scope
}

// newFixture is the package database, emptied.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	bootstrapOnce.Do(func() { bootstrapErr = schema.Bootstrap(ctx, pool) })
	if bootstrapErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootstrapErr)
	}
	f := newFixtureOn(t, ctx, pool, "testdb")
	f.exec(`TRUNCATE sage.change_review, sage.query_store, sage.incidents,
		sage.findings CASCADE`)
	f.exec(`DELETE FROM sage.sre_change_events`)
	return f
}

// newFixtureOn binds the receipts fixture to an already bootstrapped pool
// (an rv fixture copy).
func newFixtureOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: ctx, pool: pool, name: name, clock: &clock{},
		cfg: config.DefaultChangeReviewConfig()}
	f.newService()
	f.bindFeed()
	return f
}

// newService (re)builds the service over the fixture's config and clock.
func (f *fixture) newService() {
	f.t.Helper()
	svc, err := changereview.New(changereview.Deps{Database: f.name, Pool: f.pool,
		Config: f.cfg, Analyzer: nopAnalyzer{}, Now: f.clock.Now, Logf: f.t.Logf})
	if err != nil {
		f.t.Fatalf("changereview.New: %v", err)
	}
	f.svc = svc
}

func (f *fixture) bindFeed() {
	f.t.Helper()
	st, err := sre.NewPostgresStore(f.pool, sre.DefaultLimits())
	if err != nil {
		f.t.Fatalf("sre store: %v", err)
	}
	dep, err := st.EnsureDeployment(f.ctx)
	if err != nil {
		f.t.Fatalf("deployment: %v", err)
	}
	f.scope, err = st.BindDatabase(f.ctx, sre.Binding{DeploymentID: dep,
		RuntimeKey: "receipt-test:" + string(sre.NewUUID()),
		Strength:   sre.StrengthConfigured, ClusterEpoch: "unknown"})
	if err != nil {
		f.t.Fatalf("bind database: %v", err)
	}
	f.events, err = changefeed.NewStore(f.pool)
	if err != nil {
		f.t.Fatalf("change feed store: %v", err)
	}
	scope := f.scope
	f.feed = changefeed.NewFeed(f.events, f.name, func(context.Context) (sre.Scope, error) {
		return scope, nil
	})
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// job is a receipts job on the fixture's clock.
func (f *fixture) job() *Job {
	return NewJob(Deps{Now: f.clock.Now, Logf: f.t.Logf})
}

// db is the fixture's database as the job sees it.
func (f *fixture) db() Database {
	return Database{Name: f.name, Service: f.svc, Pool: f.pool, Changes: f.feed}
}

// run runs one job cycle over the fixture's database and fails on error.
func (f *fixture) run() Report {
	f.t.Helper()
	rep, err := f.job().RunDatabase(f.ctx, f.db())
	if err != nil {
		f.t.Fatalf("RunDatabase: %v", err)
	}
	return rep
}

func (f *fixture) get(id string) changereview.Review {
	f.t.Helper()
	r, err := f.svc.Store().Get(f.ctx, id)
	if err != nil {
		f.t.Fatalf("Get %s: %v", id, err)
	}
	return r
}

// nopAnalyzer satisfies the service's required Analyzer; no test submits.
type nopAnalyzer struct{}

func (nopAnalyzer) Analyze(context.Context, changereview.Input) (changereview.Analysis,
	error) {
	return changereview.Analysis{ParseMode: changereview.ParseAST}, nil
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func ptr[T any](v T) *T { return &v }
