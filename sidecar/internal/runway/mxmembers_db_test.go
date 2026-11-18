package runway

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The multixact member space on real PostgreSQL (guard-02a): row locks
// shared by several sessions create multixacts whose members the sampler
// counts, the rate comes from successive checkpoints, a role that may not
// execute pg_get_multixact_members gets unknown with the grant, and the
// finding opens, holds while unknown and resolves.

func mxMonitor(t *testing.T, pool *pgxpool.Pool, database string) *Monitor {
	t.Helper()
	opts := testOptions()
	opts.Database, opts.Interval, opts.Retention = database, time.Minute, 48*time.Hour
	m, err := NewMonitor(pool, erroringRunner{}, nil, opts, func(string, string, ...any) {})
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	return m
}

// clearMXFindings removes the category's findings, so each test starts
// with none.
func clearMXFindings(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
		CategoryMXMembers); err != nil {
		t.Fatalf("clear findings: %v", err)
	}
}

func mxSamples(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.multixact_member_samples
		WHERE database = $1`, db).Scan(&n); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	return n
}

func mxFindingRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT status || ':' || severity FROM sage.findings
		WHERE category = $1 AND object_identifier = $2`, CategoryMXMembers,
		MXMembersIdentifier)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	out := map[string]int{}
	for _, s := range got {
		out[s]++
	}
	return out
}

// shareLockFixture creates 50 multixacts: one session holds FOR SHARE on
// 50 rows while another locks each row FOR SHARE in its own transaction,
// so every row gets a multixact of a distinct member pair (a backend
// reuses its multixact for the same member set inside one transaction).
func shareLockFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	table := fmt.Sprintf("mx_fixture_%d", time.Now().UnixNano())
	ident := pgx.Identifier{"public", table}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+ident+
		" AS SELECT g AS id FROM generate_series(1, 50) g"); err != nil {
		t.Fatalf("fixture table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+ident) })
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT id FROM "+ident+" FOR SHARE"); err != nil {
		t.Fatalf("holder FOR SHARE: %v", err)
	}
	for i := 1; i <= 50; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin row %d: %v", i, err)
		}
		if _, err := tx.Exec(ctx, "SELECT id FROM "+ident+" WHERE id = $1 FOR SHARE",
			i); err != nil {
			t.Fatalf("row %d FOR SHARE: %v", i, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit row %d: %v", i, err)
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the holder: %v", err)
	}
}

func checkpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "CHECKPOINT"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

func TestMXMembers_DB_SharedRowLocksGiveAPositiveEstimateAndRate(t *testing.T) {
	pool, ctx := livePool(t)
	clearMXFindings(t, ctx, pool)
	db := fmt.Sprintf("mx_happy_%d", time.Now().UnixNano())
	m := mxMonitor(t, pool, db)
	checkpoint(t, ctx, pool)
	first, err := m.tickMXMembers(ctx, clusterScope{})
	if err != nil || first == nil || first.Estimate.Status != MXMeasured || !first.Recorded {
		t.Fatalf("first reading = %+v, %v; want a recorded measurement", first, err)
	}
	if !math.IsNaN(first.Runway.RatePerS) {
		t.Fatalf("the first reading has rate %v, want none yet", first.Runway.RatePerS)
	}
	shareLockFixture(t, ctx, pool)
	time.Sleep(1100 * time.Millisecond) // checkpoint_time has one-second resolution
	checkpoint(t, ctx, pool)
	res, err := m.tickMXMembers(ctx, clusterScope{})
	if err != nil {
		t.Fatalf("second reading: %v", err)
	}
	e := res.Estimate
	if e.Status != MXMeasured || e.Live < 50 || e.Sampled < 50 || e.Members < 100 ||
		e.MeanMembers < 2 || e.NextMXID-first.Estimate.NextMXID < 50 ||
		e.NextOffset-first.Estimate.NextOffset < 100 {
		t.Fatalf("estimate after the fixture = %+v (first %+v)", e, first.Estimate)
	}
	if !(res.Runway.RatePerS > 0) || res.Runway.Severity != "" || res.Opened {
		t.Fatalf("runway = %+v, want a positive rate and no finding", res.Runway)
	}
	if n := mxSamples(t, ctx, pool, db); n != 2 {
		t.Fatalf("%d samples stored, want 2", n)
	}
	if rows := mxFindingRows(t, ctx, pool); len(rows) != 0 {
		t.Fatalf("findings %v, want none", rows)
	}
}

// restrictedPool connects as a new login role without superuser.
func restrictedPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Pool,
	string) {
	t.Helper()
	role := fmt.Sprintf("runway_mx_%d", os.Getpid())
	ident := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident+"; CREATE ROLE "+ident+
		" LOGIN PASSWORD 'runway-test'"); err != nil {
		t.Fatalf("role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+ident+"; DROP ROLE "+ident)
	})
	u, _ := url.Parse(testdb.SkipUnlessLive(t))
	u.User = url.UserPassword(role, "runway-test")
	restricted, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(restricted.Close)
	return restricted, role
}

func TestMXMembers_DB_PermissionDeniedIsUnknownWithTheGrant(t *testing.T) {
	pool, ctx := livePool(t)
	restricted, role := restrictedPool(t, ctx, pool)
	e, err := measureMXMembers(ctx, restricted)
	if err != nil || e.Status != MXMeasured {
		t.Fatalf("with the default grants = %+v, %v; want measured", e, err)
	}
	for _, fn := range []string{mxMembersFunc, mxControlFunc} {
		if _, err := pool.Exec(ctx, "REVOKE EXECUTE ON FUNCTION "+fn+" FROM PUBLIC"); err != nil {
			t.Fatalf("revoke %s: %v", fn, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "GRANT EXECUTE ON FUNCTION "+fn+
				" TO PUBLIC")
		})
		e, err := measureMXMembers(ctx, restricted)
		if err != nil {
			t.Fatalf("revoked %s: err %v, want unknown", fn, err)
		}
		want := "GRANT EXECUTE ON FUNCTION " + fn + " TO " + role
		if e.Status != MXUnknown || !strings.Contains(e.Grant, want) ||
			!strings.Contains(e.Reason, fn) {
			t.Fatalf("revoked %s: estimate %+v, want unknown naming %q", fn, e, want)
		}
	}
}

// scriptedMX replays readings in order, then repeats the last.
type scriptedMX struct {
	mu   sync.Mutex
	list []MXMemberEstimate
	i    int
}

func (s *scriptedMX) read(context.Context) (MXMemberEstimate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.list[min(s.i, len(s.list)-1)]
	s.i++
	return e, nil
}

func measuredAt(at time.Time, offset uint32, members float64, live uint32) MXMemberEstimate {
	return MXMemberEstimate{MXControl: MXControl{NextMXID: 1 + live, NextOffset: offset,
		OldestMXID: 1, CheckpointAt: at}, Status: MXMeasured, Live: live,
		Sampled: int(min(live, MXMemberSampleMax)), Members: members}
}

var mxTransitionSteps = []struct {
	name     string
	recorded bool
	findings map[string]int
}{
	{"unknown", false, map[string]int{}},
	{"measured", true, map[string]int{}},
	{"warning", true, map[string]int{"open:warning": 1}},
	{"unknown holds the finding", false, map[string]int{"open:warning": 1}},
	{"resolved after the freeze", true, map[string]int{"resolved:warning": 1}},
}

// TestMXMembers_DB_StateTransitions: unknown -> measured -> warning ->
// held while unknown -> resolved once a freeze empties the member space.
// The readings are scripted (a real cluster-wide freeze would need
// template0 frozen on the shared test server); the samples, the rate and
// the finding are real.
func TestMXMembers_DB_StateTransitions(t *testing.T) {
	pool, ctx := livePool(t)
	clearMXFindings(t, ctx, pool)
	db := fmt.Sprintf("mx_states_%d", time.Now().UnixNano())
	m := mxMonitor(t, pool, db)
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	// 1000 members/s; 10 days before the safe threshold is a warning.
	near := MXMemberSafeThreshold - 1000*86400*10
	s := &scriptedMX{list: []MXMemberEstimate{
		{Status: MXUnknown, Reason: "role sage may not execute x", Grant: "GRANT x"},
		measuredAt(t0, 1000, 5000, 2000),
		measuredAt(t0.Add(time.Hour), 1000+3600000, near, 400000),
		{Status: MXUnknown, Reason: "role sage may not execute x", Grant: "GRANT x"},
		measuredAt(t0.Add(2*time.Hour), 1000+7200000, 0, 0),
	}}
	m.mxRead = s.read
	for _, st := range mxTransitionSteps {
		res, err := m.tickMXMembers(ctx, clusterScope{})
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if res.Recorded != st.recorded {
			t.Fatalf("%s: recorded %v, want %v (%+v)", st.name, res.Recorded, st.recorded, res)
		}
		if got := mxFindingRows(t, ctx, pool); fmt.Sprint(got) != fmt.Sprint(st.findings) {
			t.Fatalf("%s: findings %v, want %v (runway %+v)", st.name, got, st.findings,
				res.Runway)
		}
		if st.name == "warning" && math.Abs(res.Runway.RatePerS-1000) > 1e-6 {
			t.Fatalf("warning: rate %v, want 1000/s from the stored samples",
				res.Runway.RatePerS)
		}
		if st.name == "resolved after the freeze" && !res.Resolved {
			t.Fatalf("resolved: %+v does not say it resolved the finding", res)
		}
	}
	if n := mxSamples(t, ctx, pool, db); n != 3 {
		t.Fatalf("%d samples, want 3 (unknown readings record none)", n)
	}
}

func TestMXMembers_DB_IntervalKeepsTheLastReading(t *testing.T) {
	pool, ctx := livePool(t)
	db := fmt.Sprintf("mx_interval_%d", time.Now().UnixNano())
	m := mxMonitor(t, pool, db)
	m.opts.MXMemberInterval = 5 * time.Minute
	now := time.Now()
	m.now = func() time.Time { return now }
	s := &scriptedMX{list: []MXMemberEstimate{measuredAt(now, 10, 0, 0),
		measuredAt(now.Add(time.Minute), 20, 0, 0)}}
	m.mxRead = s.read
	a, errA := m.tickMXMembers(ctx, clusterScope{})
	now = now.Add(5*time.Minute - time.Second)
	b, errB := m.tickMXMembers(ctx, clusterScope{})
	now = now.Add(time.Second)
	c, errC := m.tickMXMembers(ctx, clusterScope{})
	if errA != nil || errB != nil || errC != nil {
		t.Fatalf("ticks: %v, %v, %v", errA, errB, errC)
	}
	if a != b || c == b || s.i != 2 || mxSamples(t, ctx, pool, db) != 2 {
		t.Fatalf("readings %d, samples %d; want the second tick to reuse the first",
			s.i, mxSamples(t, ctx, pool, db))
	}
	if c.Estimate.NextOffset != 20 {
		t.Fatalf("third tick read offset %d, want 20", c.Estimate.NextOffset)
	}
}

// TestMXMembers_DB_ConcurrentSamplers: two monitors of one database and
// two goroutines on each sample at once; every sample is stored and one
// finding is open.
func TestMXMembers_DB_ConcurrentSamplers(t *testing.T) {
	pool, ctx := livePool(t)
	clearMXFindings(t, ctx, pool)
	db := fmt.Sprintf("mx_race_%d", time.Now().UnixNano())
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	near := MXMemberSafeThreshold - 1000*86400*2 // two days: critical
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 2; i++ {
		m := mxMonitor(t, pool, db)
		s := &scriptedMX{}
		for k := 0; k < 5; k++ {
			at := t0.Add(time.Duration(k+1) * time.Minute)
			s.list = append(s.list, measuredAt(at, uint32(60000*(k+1)), near, 1000))
		}
		m.mxRead = s.read
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 5; k++ {
					if _, err := m.tickMXMembers(ctx, clusterScope{}); err != nil {
						errs <- err
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("tick: %v", err)
	}
	if n := mxSamples(t, ctx, pool, db); n != 20 {
		t.Fatalf("%d samples, want 20", n)
	}
	if got := mxFindingRows(t, ctx, pool); got["open:critical"] != 1 || len(got) != 1 {
		t.Fatalf("findings %v, want one open critical", got)
	}
}

func TestMXMembers_DB_TickReportsTheReading(t *testing.T) {
	pool, ctx := livePool(t)
	m := mxMonitor(t, pool, fmt.Sprintf("mx_tick_%d", time.Now().UnixNano()))
	res, _ := m.Tick(ctx) // the erroring runner fails every probe
	if res.MXMembers == nil || res.MXMembers.Estimate.Status != MXMeasured ||
		!res.MXMembers.Recorded {
		t.Fatalf("tick reading = %+v, want a recorded measurement", res.MXMembers)
	}
}
