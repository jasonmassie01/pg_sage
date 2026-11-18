package scorecard

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/review/reviewtest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/scorecard"))
}

// livePool bootstraps the fixture database twice and empties the review
// and plan tables.
func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	if _, err := pool.Exec(ctx, `TRUNCATE sage.review_run, sage.plan,
		sage.clone_instances CASCADE`); err != nil {
		t.Fatalf("empty the review tables: %v", err)
	}
	return pool, ctx
}

func seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, r reviewtest.Review) int64 {
	t.Helper()
	id, err := reviewtest.SeedReview(ctx, pool, r)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Integration: seeded plans, steps and outcomes give the scorecard numbers,
// a running review and another database's review are left out, and a
// fixture database is marked.
func TestStoreDB_SeededOutcomesGiveTheNumbers(t *testing.T) {
	pool, ctx := livePool(t)
	seed(t, ctx, pool, reviewtest.Review{Database: "shop", Models: []string{"OpenAI/M1"},
		Steps: []reviewtest.Outcome{
			{Origin: "generator", Ref: "indexes", Verdict: "improved", Accepted: true},
			{Origin: "model", Ref: "m1", Verdict: "regressed", Accepted: true,
				Reverted: true},
			{Origin: "model", Ref: "m1"},
		}})
	seed(t, ctx, pool, reviewtest.Review{Database: "shop", Models: []string{"m1"}})
	seed(t, ctx, pool, reviewtest.Review{Database: "shop", Status: "running"})
	seed(t, ctx, pool, reviewtest.Review{Database: "other", Models: []string{"m1"},
		Steps: []reviewtest.Outcome{{Origin: "model", Ref: "m1", Verdict: "improved",
			Accepted: true}}})
	sc, err := NewStore(pool).Read(ctx, "shop", time.Time{}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if sc.Reviews != 2 {
		t.Fatalf("reviews = %d, want 2 (running and other database left out)", sc.Reviews)
	}
	m, g := sc.ByOrigin[OriginModel], sc.ByOrigin[OriginGenerator]
	if m.Proposed != 2 || m.Accepted != 1 || m.Regressed != 1 || m.Reverted != 1 ||
		m.Pending != 1 || m.Decided != 1 || m.Improved != 0 {
		t.Fatalf("model counts = %+v", m)
	}
	if g.Proposed != 1 || g.Improved != 1 || g.Decided != 1 ||
		sc.ByGenerator["indexes"].Improved != 1 {
		t.Fatalf("generator counts = %+v (%+v)", g, sc.ByGenerator)
	}
	first := sc.PerReview[1]
	if strings.Join(first.Run.Models, ",") != "m1" || first.Run.Fixture ||
		first.Run.Status != StatusDone {
		t.Fatalf("first review run = %+v, want model m1 normalized, not a fixture", first.Run)
	}
}

func TestStoreDB_SinceAndFixturesNarrowTheRead(t *testing.T) {
	pool, ctx := livePool(t)
	seed(t, ctx, pool, reviewtest.Review{Database: "bench1", Models: []string{"m1"}})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.clone_instances (deployment_id, adapter,
		scope, name, purpose, status, expires_at) VALUES (gen_random_uuid(), 'fake', 'x',
		'clone7', 'gameday', 'ready', now() + interval '1 day')`); err != nil {
		t.Fatalf("seed a clone receipt: %v", err)
	}
	seed(t, ctx, pool, reviewtest.Review{Database: "clone7", Models: []string{"m1"}})
	for db, fixtures := range map[string][]string{"bench1": {"bench1"}, "clone7": nil} {
		sc, err := NewStore(pool).Read(ctx, db, time.Time{}, fixtures)
		if err != nil || sc.Reviews != 1 || !sc.PerReview[0].Run.Fixture {
			t.Fatalf("%s: %+v %v, want one review marked as a fixture", db, sc, err)
		}
	}
	sc, err := NewStore(pool).Read(ctx, "bench1", time.Now().Add(time.Hour), nil)
	if err != nil || sc.Reviews != 0 {
		t.Fatalf("since the future: %+v %v, want no reviews", sc, err)
	}
}

func TestStoreDB_MissingTableIsAnErrorNamingTheQuery(t *testing.T) {
	pool, ctx := livePool(t)
	seed(t, ctx, pool, reviewtest.Review{Database: "shop", Models: []string{"m1"},
		Steps: []reviewtest.Outcome{{Origin: "model", Ref: "m1"}}})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sage.plan_step_origin RENAME TO pso_gone`); err !=
		nil {
		t.Fatal(err)
	}
	_, err = NewStore(tx).Read(ctx, "shop", time.Time{}, nil)
	if err == nil || !strings.Contains(err.Error(), `"review steps"`) {
		t.Fatalf("error = %v, want the review steps query named", err)
	}
}
