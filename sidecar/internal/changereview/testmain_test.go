package changereview

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Fixtures for Change Review: one disposable database per package process
// with the sage schema bootstrapped once; every test starts from empty
// change_review tables. Stages are fakes (fakes_test.go); nothing here
// reaches an LLM provider.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/changereview"))
}

var (
	bootstrapOnce sync.Once
	bootstrapErr  error
)

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	f := &fixture{t: t, ctx: ctx, pool: pool}
	f.exec(`TRUNCATE sage.change_review, sage.change_review_budget_day`)
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// testConfig is the default configuration; tests adjust it.
func testConfig() config.ChangeReviewConfig { return config.DefaultChangeReviewConfig() }

// deps is a service over the fixture with the given analyzer; tests
// adjust the result before New.
func (f *fixture) deps(a Analyzer) Deps {
	return Deps{Database: "testdb", Pool: f.pool, Config: testConfig(), Analyzer: a,
		Logf: f.t.Logf}
}

// service builds and starts a service.
func (f *fixture) service(d Deps) *Service {
	f.t.Helper()
	s, err := New(d)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	if _, err := s.Start(f.ctx); err != nil {
		f.t.Fatalf("Start: %v", err)
	}
	return s
}

var (
	cliCaller = Caller{Actor: "mcp:token:ci", Source: SourceCLI}
	apiCaller = Caller{Actor: "user:7", Source: SourceAPI}
)

func ptr[T any](v T) *T { return &v }

// utc is t truncated to the store's precision.
func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
