package candidate

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
	os.Exit(testdb.Run(m.Run, "internal/review/candidate"))
}

// livePool connects to the package's fixture database, bootstraps the sage
// schema twice (the second run must be a clean no-op) and empties the
// review tables.
func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for i := 0; i < 2; i++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", i+1, err)
		}
	}
	if _, err := pool.Exec(ctx, "TRUNCATE sage.review_run CASCADE"); err != nil {
		t.Fatalf("empty the review tables: %v", err)
	}
	return pool, ctx
}

// newRun inserts a review run of db and returns its id.
func newRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.review_run
		(database_name, kind, rubric_version) VALUES ($1, 'review', 'pg_sage-rubric/1')
		RETURNING id`, db).Scan(&id)
	if err != nil {
		t.Fatalf("insert a review run: %v", err)
	}
	return id
}
