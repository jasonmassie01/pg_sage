package recommendation

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/recommendation"))
}

var (
	testPoolOnce sync.Once
	testPool     *pgxpool.Pool
	testPoolErr  error
	uniqueSeq    atomic.Int64
)

// requireDB returns a bootstrapped pool on the designated test server.
func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	testPoolOnce.Do(func() {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = err
			return
		}
		cfg.MaxConns = 8
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, cfg)
		if testPoolErr == nil {
			testPoolErr = schema.Bootstrap(ctx, testPool)
		}
	})
	if testPoolErr != nil {
		t.Fatalf("test database: %v", testPoolErr)
	}
	return testPool, ctx
}

// uniqueDB is a database identity no other test uses, so tests share
// one fixture database without seeing each other's rows.
func uniqueDB(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), uniqueSeq.Add(1))
}

// seedFinding inserts an open finding and returns its id.
func seedFinding(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	category, target, forward, inverse string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, rollback_sql, status)
		VALUES ($1, 'warning', 'index', $2, 'test finding', '{}', 'do it',
		        $3, NULLIF($4, ''), 'open') RETURNING id`,
		category, target, forward, inverse).Scan(&id)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return id
}

// proposal builds a proposal whose finding row exists.
func proposal(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, db, forward, inverse string,
) Proposal {
	t.Helper()
	category := "rec_test_" + db
	target := "public.orders_" + db
	seedFinding(t, ctx, pool, category, target, forward, inverse)
	return Proposal{
		DatabaseName: db, Category: category, Target: target,
		ObjectType: "index", Title: "add index", Severity: "warning",
		ActionRisk: "safe", Recommendation: "index it",
		ForwardSQL: forward, InverseSQL: inverse,
		Evidence: map[string]any{"seq_scans": 10},
	}
}

func mustPropose(t *testing.T, ctx context.Context, s *Store, p Proposal) ProposeResult {
	t.Helper()
	res, err := s.Propose(ctx, p)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return res
}

func mustGet(t *testing.T, ctx context.Context, s *Store, id int64) Recommendation {
	t.Helper()
	rec, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get %d: %v", id, err)
	}
	return rec
}

// setState forces a head into a state for a test precondition. It writes
// the row directly, the way a crash or an earlier process would leave it.
func setState(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, state State,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `UPDATE sage.recommendation
		SET state = $2,
		    approved_revision = CASE WHEN $2 IN ('proposed') THEN NULL
		        ELSE COALESCE(approved_revision, revision) END,
		    approved_hash = CASE WHEN $2 IN ('proposed') THEN NULL
		        ELSE COALESCE(approved_hash, content_hash) END,
		    approved_by = CASE WHEN $2 IN ('proposed') THEN NULL
		        ELSE COALESCE(approved_by, 'user:1') END,
		    approved_at = CASE WHEN $2 IN ('proposed') THEN NULL
		        ELSE COALESCE(approved_at, now()) END,
		    lease_until = CASE WHEN $2 = 'applying'
		        THEN now() + interval '1 hour' ELSE NULL END
		WHERE id = $1`, id, string(state))
	if err != nil {
		t.Fatalf("set state %s: %v", state, err)
	}
}

func transitionsOf(t *testing.T, ctx context.Context, s *Store, id int64) []Transition {
	t.Helper()
	history, err := s.Transitions(ctx, id)
	if err != nil {
		t.Fatalf("transitions %d: %v", id, err)
	}
	return history
}

func lastTransition(t *testing.T, ctx context.Context, s *Store, id int64) Transition {
	t.Helper()
	history := transitionsOf(t, ctx, s, id)
	if len(history) == 0 {
		t.Fatalf("recommendation %d has no transitions", id)
	}
	return history[len(history)-1]
}

// insertActionLog writes an action_log row with the given outcome.
func insertActionLog(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, outcome, sql string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('create_index', $1, $2)
		RETURNING id`, sql, outcome).Scan(&id)
	if err != nil {
		t.Fatalf("insert action_log: %v", err)
	}
	return id
}

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
