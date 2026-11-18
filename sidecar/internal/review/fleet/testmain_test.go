package fleet

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/fleet"))
}

// livePool is a fresh database of its own, bootstrapped twice (the second
// run must be a clean no-op).
func livePool(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	dsn := testdb.CreateDatabase(t, "fleetrev_"+label)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for i := 0; i < 2; i++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d of %s: %v", i+1, label, err)
		}
	}
	return pool, ctx
}

// gradedRun stores a finished review run of db with score.
func gradedRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
	score float64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version, status, overall_grade, overall_score, coverage_pct, finished_at)
		VALUES ($1, 'review', 'pg_sage-rubric/1', 'done', $2, $3, 80, now())
		RETURNING id`, db, letterOf(score), score).Scan(&id)
	if err != nil {
		t.Fatalf("store a graded run of %s: %v", db, err)
	}
	return id
}

func letterOf(score float64) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 80:
		return "B"
	case score >= 70:
		return "C"
	case score >= 60:
		return "D"
	}
	return "F"
}
