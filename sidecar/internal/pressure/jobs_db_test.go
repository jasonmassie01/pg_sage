package pressure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// guard-01b on a real server (PG14 and PG17): the job store, the writing
// backend probe, the approval item, and the whole path from two stops to
// a fixture backfill cancelled after a person approves.

func jobsPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Pool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func uniqueApp(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

func TestPressureJobStore_RegisterReRegisterEndAndExpire(t *testing.T) {
	pool, ctx := jobsPool(t)
	s := NewJobStore(pool)
	app := uniqueApp("pj_store")
	j, err := s.Register(ctx, JobSpec{Database: "orders", ApplicationName: app,
		Table: "public.orders", RegisteredBy: "user:1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if j.ID <= 0 || j.Enforce || j.Table != "public.orders" || j.RegisteredBy != "user:1" ||
		j.ExpiresAt.Sub(j.CreatedAt) != time.Hour {
		t.Fatalf("registered job = %+v", j)
	}
	again, err := s.Register(ctx, JobSpec{Database: "orders", ApplicationName: app,
		RegisteredBy: "token:9", Enforce: true, TTL: 2 * time.Hour})
	if err != nil || again.ID != j.ID || !again.Enforce || again.RegisteredBy != "token:9" ||
		again.Table != "" || !again.ExpiresAt.After(j.ExpiresAt) {
		t.Fatalf("re-registration = %+v, %v; want job %d updated", again, err, j.ID)
	}
	other, err := s.Register(ctx, JobSpec{Database: "billing", ApplicationName: app,
		RegisteredBy: "user:1"})
	if err != nil || other.ID == j.ID {
		t.Fatalf("same name in another database = %+v, %v; want its own job", other, err)
	}
	active, err := s.Active(ctx, "orders")
	if err != nil || !hasJob(active, j.ID) || hasJob(active, other.ID) {
		t.Fatalf("active(orders) = %+v, %v", active, err)
	}
	if _, err := s.End(ctx, "billing", j.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("end from another database = %v, want ErrJobNotFound", err)
	}
	ended, err := s.End(ctx, "orders", j.ID)
	if err != nil || ended.ExpiresAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("end = %+v, %v", ended, err)
	}
	if active, _ = s.Active(ctx, "orders"); hasJob(active, j.ID) {
		t.Fatalf("an ended job is still active: %+v", active)
	}
	assertEnded(t, ctx, s, ended, app)
}

// assertEnded: an ended job still reads by id, an unknown id is not
// found, and a registration after the end is a new job.
func assertEnded(t *testing.T, ctx context.Context, s *JobStore, ended Job, app string) {
	t.Helper()
	got, err := s.Get(ctx, ended.ID)
	if err != nil || got.ID != ended.ID || !got.ExpiresAt.Equal(ended.ExpiresAt) {
		t.Fatalf("get ended job = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, 987654321); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("get unknown = %v, want ErrJobNotFound", err)
	}
	fresh, err := s.Register(ctx, JobSpec{Database: "orders", ApplicationName: app,
		RegisteredBy: "user:1"})
	if err != nil || fresh.ID == ended.ID {
		t.Fatalf("register after end = %+v, %v; want a new job", fresh, err)
	}
}

func TestPressureJobStore_ConcurrentRegistrationsMakeOneJob(t *testing.T) {
	pool, ctx := jobsPool(t)
	s := NewJobStore(pool)
	app := uniqueApp("pj_race")
	ids := make(chan int64, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.Register(ctx, JobSpec{Database: "orders", ApplicationName: app,
				RegisteredBy: "user:1"})
			if err != nil {
				t.Errorf("register: %v", err)
				return
			}
			ids <- j.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("8 concurrent registrations made jobs %v, want one", seen)
	}
}

func TestPressureJobStore_RefusesInvalidSpec(t *testing.T) {
	pool, ctx := jobsPool(t)
	_, err := NewJobStore(pool).Register(ctx, JobSpec{Database: "orders",
		ApplicationName: "x'; DROP TABLE sage.pressure_jobs; --\n", RegisteredBy: "user:1"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("register = %v, want ErrInvalidRequest", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class
		WHERE oid = 'sage.pressure_jobs'::regclass`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sage.pressure_jobs is gone (%d, %v)", n, err)
	}
}

func hasJob(jobs []Job, id int64) bool {
	for _, j := range jobs {
		if j.ID == id {
			return true
		}
	}
	return false
}

// fixtureSession runs sql on its own connection named app; done receives
// the statement's error.
type fixtureSession struct {
	conn *pgx.Conn
	pid  int32
	done chan error
}

func startFixture(t *testing.T, ctx context.Context, app, sql string) *fixtureSession {
	t.Helper()
	cfg, err := pgx.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.RuntimeParams["application_name"] = app
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	f := &fixtureSession{conn: conn, done: make(chan error, 1)}
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&f.pid); err != nil {
		t.Fatalf("fixture pid: %v", err)
	}
	if sql == "" {
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return f
	}
	// A pgx.Conn is not safe for concurrent use: the cleanup cancels the
	// statement and waits for its goroutine before it closes the conn.
	execCtx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-finished
		_ = conn.Close(context.Background())
	})
	go func() {
		defer close(finished)
		_, err := conn.Exec(execCtx, sql)
		f.done <- err
	}()
	return f
}

func awaitWriting(t *testing.T, ctx context.Context, b *SQLBackends, app string,
	pid int32) Backend {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		got, err := b.Writing(ctx, app)
		if err != nil {
			t.Fatalf("writing: %v", err)
		}
		for _, w := range got {
			if w.PID == pid {
				return w
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid %d never showed as writing", pid)
	return Backend{}
}

func TestPressureJobBackends_ListOnlyWritingSessionsOfTheJob(t *testing.T) {
	pool, ctx := jobsPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pj_target (id int)")
	app := uniqueApp("pj_probe")
	b := NewSQLBackends(pool)
	writer := startFixture(t, ctx, app, "INSERT INTO pj_target VALUES (1); "+
		"SELECT pg_sleep(20)")
	reader := startFixture(t, ctx, app, "SELECT pg_sleep(20)")
	idle := startFixture(t, ctx, app, "")
	other := startFixture(t, ctx, app+"_other", "INSERT INTO pj_target VALUES (2); "+
		"SELECT pg_sleep(20)")
	got := awaitWriting(t, ctx, b, app, writer.pid)
	if got.ApplicationName != app || got.User == "" || got.Database == "" ||
		len(got.QueryHash) != 64 || got.QueryStart.Before(got.BackendStart) ||
		got.ObservedAt.IsZero() {
		t.Fatalf("writing backend = %+v", got)
	}
	all, err := b.Writing(ctx, app)
	if err != nil {
		t.Fatalf("writing: %v", err)
	}
	for _, w := range all {
		if w.PID == reader.pid || w.PID == idle.pid || w.PID == other.pid {
			t.Fatalf("pid %d is listed but is not a writing session of %s", w.PID, app)
		}
	}
	re, ok, err := b.Recheck(ctx, app, writer.pid, got.BackendStart)
	if err != nil || !ok || re.QueryHash != got.QueryHash {
		t.Fatalf("recheck = %+v %v %v", re, ok, err)
	}
	if _, ok, err := b.Recheck(ctx, app, writer.pid,
		got.BackendStart.Add(-time.Second)); err != nil || ok {
		t.Fatalf("recheck of another incarnation = %v %v, want no match", ok, err)
	}
	if _, ok, err := b.Recheck(ctx, app+"_other", writer.pid, got.BackendStart); err != nil ||
		ok {
		t.Fatalf("recheck under another name = %v %v, want no match", ok, err)
	}
	none, err := b.Writing(ctx, uniqueApp("pj_nobody"))
	if err != nil || len(none) != 0 {
		t.Fatalf("writing(nobody) = %+v, %v; want none", none, err)
	}
}

// stopLimits stop on transaction ID age above 1, which every live server
// has: the fixture's pressure reads stop on PG14 and PG17 alike.
func stopLimits(context.Context) (Limits, error) {
	l := DefaultLimits(Alerts{})
	l.Inputs[policy.PressureXIDAge] = Limit{Stop: ptr(1)}
	return l, nil
}

type liveEnforcement struct {
	jobs     *JobStore
	enforcer *Enforcer
	exec     *executor.Executor
	actions  *store.ActionStore
}

func newLiveEnforcement(t *testing.T, pool *pgxpool.Pool, database string) *liveEnforcement {
	t.Helper()
	checker := fastChecker(pool, stopLimits)
	jobs, backends := NewJobStore(pool), NewSQLBackends(pool)
	e, err := NewEnforcer(EnforcerOptions{Database: database, Jobs: jobs, Pressure: checker,
		Backends: backends, Proposals: NewPGProposer(pool, nil)})
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	ex := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	ex.EnableStandingPolicyDocument(policy.StaffedProfile(), nil)
	ex.WithEmergencyStopCheck(func(context.Context) bool { return false })
	r, err := NewRunner(RunnerOptions{Jobs: jobs, Pressure: checker, Backends: backends,
		Exec: ex})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := ex.AddApprovedActionRunner(RunnerName, []string{JobIdentityPrefix}, r); err != nil {
		t.Fatalf("add the pressure job runner: %v", err)
	}
	return &liveEnforcement{jobs: jobs, enforcer: e, exec: ex,
		actions: store.NewActionStore(pool)}
}

// tickUntilProposed ticks (each a fresh read set) until a proposal exists.
func (le *liveEnforcement) tickUntilProposed(t *testing.T, ctx context.Context) int {
	t.Helper()
	total := 0
	for i := 0; i < 6 && total == 0; i++ {
		time.Sleep(30 * time.Millisecond) // past the 20 ms cache
		res, err := le.enforcer.Tick(ctx)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if i == 0 && res.Proposed != 0 {
			t.Fatalf("the first stop proposed %d", res.Proposed)
		}
		total += res.Proposed
	}
	return total
}

func queueItemFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	key string) store.QueuedAction {
	t.Helper()
	var id int
	if err := pool.QueryRow(ctx, `SELECT id FROM sage.action_queue WHERE identity_key = $1`,
		key).Scan(&id); err != nil {
		t.Fatalf("queue item %s: %v", key, err)
	}
	a, err := store.NewActionStore(pool).GetByID(ctx, id)
	if err != nil || a == nil {
		t.Fatalf("read queue item %d: %v", id, err)
	}
	return *a
}

// Integration and state transitions: registered -> stop -> proposed ->
// approved -> cancelled -> job expired, on a fixture backfill.
func TestPressureJobEnforcement_CancelsTheBackfillAfterApproval(t *testing.T) {
	pool, ctx := jobsPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pj_target (id int)")
	app := uniqueApp("pj_live")
	le := newLiveEnforcement(t, pool, "fixture")
	job, err := le.jobs.Register(ctx, JobSpec{Database: "fixture", ApplicationName: app,
		Table: "public.pj_target", RegisteredBy: "user:1", Enforce: true, TTL: time.Hour})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	writer := startFixture(t, ctx, app, "INSERT INTO pj_target VALUES (1); "+
		"SELECT pg_sleep(30)")
	b := awaitWriting(t, ctx, NewSQLBackends(pool), app, writer.pid)
	if n := le.tickUntilProposed(t, ctx); n != 1 {
		t.Fatalf("two stops proposed %d items, want 1", n)
	}
	if res, err := le.enforcer.Tick(ctx); err != nil || res.Proposed != 0 {
		t.Fatalf("a third stop = %+v, %v; want no new item", res, err)
	}
	key := JobIdentityKey(job.ID, writer.pid, b.BackendStart)
	item := queueItemFor(t, ctx, pool, key)
	if item.Status != "pending" || item.ActionType != "cancel_backend" ||
		item.ProposedSQL != fmt.Sprintf("SELECT pg_cancel_backend(%d)", writer.pid) {
		t.Fatalf("queue item = %+v", item)
	}
	assertCardDetail(t, ctx, pool, item.FindingID, writer.pid)
	select {
	case err := <-writer.done:
		t.Fatalf("the backfill ended before any approval: %v", err)
	default:
	}
	le.approveAndAwaitCancel(t, ctx, pool, item, writer)
	if _, err := le.jobs.End(ctx, "fixture", job.ID); err != nil {
		t.Fatalf("end job: %v", err)
	}
	if res, err := le.enforcer.Tick(ctx); err != nil || res.Checked != 0 {
		t.Fatalf("tick after the job expired = %+v, %v; want nothing checked", res, err)
	}
}

// approveAndAwaitCancel approves item as user 1, runs it through the
// executor and waits for the backfill's statement to be cancelled.
func (le *liveEnforcement) approveAndAwaitCancel(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, item store.QueuedAction, writer *fixtureSession) {
	t.Helper()
	approved, err := le.actions.Approve(ctx, item.ID, 1)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	run, err := le.exec.RunApprovedAction(ctx, *approved, 1)
	if err != nil {
		t.Fatalf("approved run: %v", err)
	}
	select {
	case err := <-writer.done:
		if err == nil || !strings.Contains(err.Error(), "canceling statement") {
			t.Fatalf("backfill error = %v, want a cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the approved cancel did not reach the backfill")
	}
	var actionType string
	if err := pool.QueryRow(ctx, `SELECT action_type FROM sage.action_log WHERE id = $1`,
		run.ActionLogID).Scan(&actionType); err != nil || actionType != "cancel_backend" {
		t.Fatalf("action_log %d = %q, %v", run.ActionLogID, actionType, err)
	}
}

func assertCardDetail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, findingID int,
	pid int32) {
	t.Helper()
	var category, raw string
	if err := pool.QueryRow(ctx, `SELECT category, detail::text FROM sage.findings
		WHERE id = $1`, findingID).Scan(&category, &raw); err != nil {
		t.Fatalf("finding %d: %v", findingID, err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("finding detail: %v", err)
	}
	target, _ := detail["signal_target"].(map[string]any)
	if category != FindingCategory || target["pid"] != float64(pid) ||
		target["backend_start"] == "" || detail["verdict"] != "stop" ||
		detail["consecutive_stops"] != float64(2) {
		t.Fatalf("finding %s detail = %v", category, detail)
	}
	in, _ := detail["input_xid_age"].(string)
	if !strings.Contains(in, "stop") {
		t.Fatalf("the card does not show the stopping input: %q (%v)", in, detail)
	}
}

// An approval after the backfill moved on refuses: nothing is cancelled.
func TestPressureJobEnforcement_RefusesAGoneBackend(t *testing.T) {
	pool, ctx := jobsPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pj_target (id int)")
	app := uniqueApp("pj_gone")
	le := newLiveEnforcement(t, pool, "fixture")
	job, err := le.jobs.Register(ctx, JobSpec{Database: "fixture", ApplicationName: app,
		RegisteredBy: "user:1", Enforce: true})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	writer := startFixture(t, ctx, app, "INSERT INTO pj_target VALUES (1); "+
		"SELECT pg_sleep(2)")
	b := awaitWriting(t, ctx, NewSQLBackends(pool), app, writer.pid)
	if n := le.tickUntilProposed(t, ctx); n != 1 {
		t.Fatalf("proposed %d, want 1", n)
	}
	if err := <-writer.done; err != nil {
		t.Fatalf("fixture batch: %v", err)
	}
	item := queueItemFor(t, ctx, pool, JobIdentityKey(job.ID, writer.pid, b.BackendStart))
	approved, err := le.actions.Approve(ctx, item.ID, 1)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, err = le.exec.RunApprovedAction(ctx, *approved, 1)
	if !errors.Is(err, executor.ErrBackendEvidenceStale) {
		t.Fatalf("run against a finished batch = %v, want stale evidence", err)
	}
	var logged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log
		WHERE finding_id = $1`, item.FindingID).Scan(&logged); err != nil || logged != 0 {
		t.Fatalf("a refused run logged %d actions (%v)", logged, err)
	}
}
