package trigger

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/trigger"))
}

// livePool is a fresh database of its own, bootstrapped twice (the second
// run must be a clean no-op).
func livePool(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	dsn := testdb.CreateDatabase(t, "trigger_"+label)
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
	return pool, ctx
}

// sqlReviewer stands in for the review runner: it records a review run
// listing the triggers' kinds, reports its id through OnStart, and
// finishes it done. hold, when set, blocks after OnStart until ctx ends
// (a sidecar killed mid-run) and leaves the row running.
type sqlReviewer struct {
	pool  *pgxpool.Pool
	mu    sync.Mutex
	calls int
	runs  [][]string
	hold  bool
	busy  bool
}

func (r *sqlReviewer) Review(ctx context.Context, run Run) (int64, error) {
	r.mu.Lock()
	r.calls++
	r.runs = append(r.runs, run.Kinds())
	hold, busy := r.hold, r.busy
	r.mu.Unlock()
	if busy {
		return 0, fmt.Errorf("a grade is running: %w", ErrBusy)
	}
	var id int64
	if err := r.pool.QueryRow(ctx, `INSERT INTO sage.review_run
		(database_name, kind, triggers, rubric_version)
		VALUES ($1, 'review', $2, 'pg_sage-rubric/1') RETURNING id`, run.Database,
		run.Kinds()).Scan(&id); err != nil {
		return 0, err
	}
	if run.OnStart != nil {
		if err := run.OnStart(ctx, id); err != nil {
			return id, err
		}
	}
	if hold {
		<-ctx.Done()
		return id, ctx.Err()
	}
	_, err := r.pool.Exec(ctx, `UPDATE sage.review_run SET status = 'done',
		finished_at = now() WHERE id = $1`, id)
	return id, err
}

func (r *sqlReviewer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// clock is a settable time for a scheduler.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func scheduler(store *Store, db, owner string, rv Reviewer, c *clock) *Scheduler {
	return &Scheduler{Store: store, Database: db, Owner: owner, Interval: 24 * time.Hour,
		Lease: 10 * time.Minute, Reviewer: rv, Now: c.Now,
		Log: func(string, string, ...any) {}}
}

// fire stores one trigger of kind fired at at (key unique per call).
func fire(t *testing.T, ctx context.Context, s *Store, db string, kind Kind,
	at time.Time) int64 {
	t.Helper()
	id, ok, err := s.Fire(ctx, Trigger{Database: db, Kind: kind,
		Key: fmt.Sprintf("%s@%d", kind, at.UnixNano()), FiredAt: at,
		Detail: map[string]any{"seeded": true}})
	if err != nil || !ok || id == 0 {
		t.Fatalf("fire %s: id %d fired %v: %v", kind, id, ok, err)
	}
	return id
}

// states is each trigger's state, run and coalesce target, by id.
func states(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[int64]string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, state, COALESCE(run_id, 0),
		COALESCE(coalesced_into, 0) FROM sage.review_trigger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id, run, into int64
		var st string
		if err := rows.Scan(&id, &st, &run, &into); err != nil {
			t.Fatal(err)
		}
		out[id] = fmt.Sprintf("%s run=%d into=%d", st, run, into)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// reviewRuns is the triggers of every review run of db, oldest first.
func reviewRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT array_to_string(triggers, ',') || ':' || status
		FROM sage.review_run WHERE database_name = $1 AND kind = 'review' ORDER BY id`, db)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func hasAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
