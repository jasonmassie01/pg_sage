//go:build integration

package plan

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	onlineRows        = 1_000_000
	onlineLockTimeout = time.Second
	// lockSlack covers scheduling on a loaded machine: a statement that
	// waits for a lock fails at lock_timeout, it never waits past it.
	lockSlack = 1500 * time.Millisecond
)

// writerStats counts the concurrent workload's operations.
type writerStats struct {
	inserts, updates, lines atomic.Int64
	maxLatency              atomic.Int64 // nanoseconds
	mu                      sync.Mutex
	errs                    []error
}

func (s *writerStats) observe(d time.Duration, err error) {
	for {
		cur := s.maxLatency.Load()
		if int64(d) <= cur || s.maxLatency.CompareAndSwap(cur, int64(d)) {
			break
		}
	}
	if err != nil {
		s.mu.Lock()
		s.errs = append(s.errs, err)
		s.mu.Unlock()
	}
}

// writerPool runs the workload without cached prepared statements: a
// cached plan's result type changes when the column does (see
// docs/migration-safety.md).
func writerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(planDBPool(t).Config().ConnString())
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("writer pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// runWorkload inserts orders, updates orders and inserts lines until ctx
// ends: the live writes the swap must not block past its lock_timeout.
func runWorkload(ctx context.Context, pool *pgxpool.Pool, stats *writerStats) *sync.WaitGroup {
	ops := []func(context.Context, *rand.Rand) error{
		func(ctx context.Context, _ *rand.Rand) error {
			_, err := pool.Exec(ctx, `INSERT INTO swap09b.orders (v) VALUES ('new')`)
			stats.inserts.Add(1)
			return err
		},
		func(ctx context.Context, r *rand.Rand) error {
			_, err := pool.Exec(ctx, `UPDATE swap09b.orders SET v = 'upd' WHERE id = $1`,
				1+r.Intn(onlineRows))
			stats.updates.Add(1)
			return err
		},
		func(ctx context.Context, r *rand.Rand) error {
			_, err := pool.Exec(ctx, `INSERT INTO swap09b.lines (order_id) VALUES ($1)`,
				1+r.Intn(onlineRows))
			stats.lines.Add(1)
			return err
		},
	}
	var wg sync.WaitGroup
	for i, op := range ops {
		wg.Add(1)
		go func(seed int64, op func(context.Context, *rand.Rand) error) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for ctx.Err() == nil {
				start := time.Now()
				err := op(ctx, r)
				if ctx.Err() != nil {
					return
				}
				stats.observe(time.Since(start), err)
				time.Sleep(2 * time.Millisecond)
			}
		}(int64(i+1), op)
	}
	return &wg
}

// TestColumnSwapOnlineUnderWrites is CHECK-01: the generated script for
// a 1M-row integer primary key, referenced by a foreign key and filled
// from its sequence, runs under a concurrent write workload. No statement
// waits past its lock_timeout, the writers are never blocked longer than
// that, and the final schema is the intended one.
func TestColumnSwapOnlineUnderWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("1M-row online swap: skipped in -short runs")
	}
	pool := planDBPool(t)
	swapSchema(t, pool,
		`CREATE TABLE swap09b.orders (id serial PRIMARY KEY, v text NOT NULL)`,
		`INSERT INTO swap09b.orders (v) SELECT 'v' FROM generate_series(1, 1000000)`,
		`CREATE TABLE swap09b.lines (id bigserial PRIMARY KEY,
			order_id int NOT NULL REFERENCES swap09b.orders (id) ON DELETE CASCADE)`,
		`INSERT INTO swap09b.lines (order_id) SELECT g FROM generate_series(1, 1000000, 10) g`,
		`ANALYZE swap09b.orders`, `ANALYZE swap09b.lines`)
	facts, err := readFacts(t, pool, "orders", "id")
	if err != nil {
		t.Fatalf("ReadSwapFacts: %v", err)
	}
	p, err := PlanColumnSwap(context.Background(), facts, SwapRequest{Schema: "swap09b",
		Table: "orders", Column: "id", TargetType: "bigint", BatchSize: 20000,
		LockTimeout: onlineLockTimeout})
	if err != nil {
		t.Fatalf("PlanColumnSwap: %v", err)
	}
	stats := &writerStats{}
	ctx, stop := context.WithCancel(context.Background())
	wg := runWorkload(ctx, writerPool(t), stats)
	last := len(p.Steps) - 1
	body := p
	body.Steps = p.Steps[:last]
	run := runSwapScript(t, pool, body)
	time.Sleep(300 * time.Millisecond) // writes after the swap reach the new column
	stop()
	wg.Wait()
	checkOnlineRun(t, run, stats)
	checkCopiedValues(t, pool)
	contract := SwapPlan{Steps: []SwapStep{p.Steps[0], p.Steps[last]}}
	runSwapScript(t, pool, contract)
	checkSwappedSchema(t, pool)
}

// checkCopiedValues runs before the contract step: every row that existed
// before the swap carries its old key in the new column, unchanged, and
// only rows written after the swap lack an old value.
func checkCopiedValues(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var differ, before int64
	err := pool.QueryRow(context.Background(), `
SELECT count(*) FILTER (WHERE id__swap_old IS NOT NULL AND id__swap_old <> id),
       count(*) FILTER (WHERE id__swap_old IS NOT NULL)
FROM swap09b.orders`).Scan(&differ, &before)
	if err != nil || differ != 0 || before < onlineRows {
		t.Fatalf("copied values: %d differ, %d rows carry an old key (want 0, >= %d): %v",
			differ, before, onlineRows, err)
	}
}

func checkOnlineRun(t *testing.T, run swapRun, stats *writerStats) {
	t.Helper()
	for name, d := range run.lockWaits {
		if d > onlineLockTimeout+lockSlack {
			t.Errorf("step %s took %v, past lock_timeout %v", name, d, onlineLockTimeout)
		}
	}
	if max := time.Duration(stats.maxLatency.Load()); max > 2*onlineLockTimeout+lockSlack {
		t.Errorf("a writer waited %v; the swap may block writes for at most about "+
			"its lock_timeout plus the swap transaction", max)
	}
	if len(stats.errs) > 0 {
		t.Errorf("%d writer errors, first: %v", len(stats.errs), stats.errs[0])
	}
	if stats.inserts.Load() == 0 || stats.updates.Load() == 0 || stats.lines.Load() == 0 {
		t.Fatalf("workload did not run: %d inserts %d updates %d lines",
			stats.inserts.Load(), stats.updates.Load(), stats.lines.Load())
	}
	t.Logf("workload: %d inserts, %d updates, %d lines, max latency %v; "+
		"%d backfill batches, %d lock retries", stats.inserts.Load(), stats.updates.Load(),
		stats.lines.Load(), time.Duration(stats.maxLatency.Load()), run.batches, run.retries)
}

// checkSwappedSchema: orders.id is a bigint NOT NULL primary key named
// orders_pkey, filled from its own bigint sequence; the foreign key is
// back, validated and enforced; nothing of the swap is left.
func checkSwappedSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var typ, def, seq, seqType, pkCols string
	var notNull, fkValid bool
	var leftovers int
	err := pool.QueryRow(ctx, `
SELECT format_type(a.atttypid, a.atttypmod), a.attnotnull,
       pg_get_expr(d.adbin, d.adrelid),
       pg_get_serial_sequence('swap09b.orders', 'id'),
       (SELECT format_type(s.seqtypid, NULL) FROM pg_sequence s
         WHERE s.seqrelid = 'swap09b.orders_id_seq'::regclass),
       (SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
         WHERE c.conrelid = a.attrelid AND c.conname = 'orders_pkey' AND c.contype = 'p'),
       (SELECT c.convalidated FROM pg_constraint c
         WHERE c.conname = 'lines_order_id_fkey' AND c.confrelid = a.attrelid),
       (SELECT count(*) FROM pg_attribute x WHERE x.attrelid = a.attrelid
          AND x.attname LIKE '%swap%' AND NOT x.attisdropped)
     + (SELECT count(*) FROM pg_trigger g WHERE g.tgrelid = a.attrelid AND NOT g.tgisinternal)
     + (SELECT count(*) FROM pg_proc WHERE proname LIKE '%swap_sync%')
FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = 'swap09b.orders'::regclass AND a.attname = 'id'`).Scan(
		&typ, &notNull, &def, &seq, &seqType, &pkCols, &fkValid, &leftovers)
	if err != nil {
		t.Fatalf("read swapped schema: %v", err)
	}
	if typ != "bigint" || !notNull || seq != "swap09b.orders_id_seq" || seqType != "bigint" ||
		pkCols != "PRIMARY KEY (id)" || !fkValid || leftovers != 0 ||
		def != "nextval('swap09b.orders_id_seq'::regclass)" {
		t.Fatalf("schema after swap: type %s notnull %v default %s seq %s/%s pk %q "+
			"fk valid %v leftovers %d", typ, notNull, def, seq, seqType, pkCols, fkValid, leftovers)
	}
	checkKeysWork(t, pool)
}

// checkKeysWork: the sequence hands out values past int4, and the foreign
// key rejects an orphan.
func checkKeysWork(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT setval('swap09b.orders_id_seq', 3000000000)`).
		Scan(&id); err != nil {
		t.Fatalf("setval: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO swap09b.orders (v) VALUES ('big') RETURNING id`).
		Scan(&id); err != nil || id != 3000000001 {
		t.Fatalf("insert past int4: id %d err %v", id, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO swap09b.lines (order_id) VALUES (-1)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("orphan line: err = %v, want foreign key violation", err)
	}
	var missing int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM swap09b.lines l
		WHERE NOT EXISTS (SELECT 1 FROM swap09b.orders o WHERE o.id = l.order_id)`).
		Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("lines without an order: %d (%v)", missing, err)
	}
}

func describeStep(s SwapStep) string { return fmt.Sprintf("%s (%s)", s.Name, s.LockLevel) }
