package selfconfig

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

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/selfconfig"))
}

var (
	poolOnce sync.Once
	sharedDB *pgxpool.Pool
	poolErr  error
)

// testPool is the package's bootstrapped fixture database; every test
// starts with an empty derivation ledger.
func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		ctx := context.Background()
		sharedDB, poolErr = pgxpool.New(ctx, dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(ctx, sharedDB)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	ctx := context.Background()
	clean := func() {
		for _, stmt := range []string{
			"DELETE FROM sage.config_derivation",
			"DELETE FROM sage.config_derived_setting",
		} {
			if _, err := sharedDB.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return sharedDB, ctx
}

// clock is a controllable time source for the engine.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestEngine(pool *pgxpool.Pool) (*Engine, *clock) {
	c := &clock{now: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	e := NewEngine(NewStore(pool))
	e.Now = c.Now
	return e, c
}

// heavyEvidence derives a value different from the default for every key.
func heavyEvidence() Evidence {
	return Evidence{
		Relations:          Known(250000),
		CatalogScanMs:      Known(300),     // query timeout 1200
		Sequences:          Known(30000),   //
		SequenceScanMs:     Known(1500),    // sequence interval 1500
		MaxConnections:     Known(1000),    // lwlock waiters 20
		TempBytesPerSecond: Known(7158279), // ~2048 MiB per 300 s: temp 8192
		CollectorCostMs:    Known(1800),    // collector interval 180
	}
}

func noOperator() map[string]bool { return map[string]bool{} }

func ledgerCount(t *testing.T, pool *pgxpool.Pool, key, event string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.config_derivation
		WHERE ($1 = '' OR key = $1) AND ($2 = '' OR event = $2)`, key, event).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func defaults() *config.Config { return config.DefaultConfig() }
