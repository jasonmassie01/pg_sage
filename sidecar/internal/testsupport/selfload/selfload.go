// Package selfload runs the same small workload from application sessions
// and from sessions configured like pg_sage's own pools (application_name
// pg_sage, every statement tagged), so a test can prove that an analysis
// input counts the application's sessions and statements only.
//
// Each group is three client backends of the test database:
//
//   - Holder: idle in transaction, holding an ACCESS EXCLUSIVE lock on the
//     group's own table.
//   - Waiter: active, waiting for a lock on the application table (behind
//     the application holder, so a pg_sage waiter is blocked by the app).
//   - Sleeper: active, in pg_sleep.
//
// Extra pg_sage sleepers can be added for counts that are cluster-wide.
//
// One admin session (application_name selfload_admin, idle between reads)
// lives as long as the workload: it creates the tables, waits for session
// states and cleans up, so no helper connection starts or ends between a
// test's two reads. At cleanup every session the workload started is
// gone from pg_stat_activity before the next test runs.
package selfload

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// AppName is the application_name of the application sessions.
const AppName = "selfload_app"

// Group is one set of sessions; the pids are the backends' pids.
type Group struct {
	Holder, Waiter, Sleeper int
	Extra                   []int // additional sleepers
}

// Pids lists every backend of the group.
func (g *Group) Pids() []int {
	return append([]int{g.Holder, g.Waiter, g.Sleeper}, g.Extra...)
}

// Workload owns the tables and sessions of one test.
type Workload struct {
	DSN       string
	AppTable  string // public.<name>, locked by the application holder
	SageTable string // public.<name>, locked by the pg_sage holder
	suffix    string
	admin     *pgx.Conn
	pids      []int    // every session started, awaited gone at cleanup
	drops     []string // fixture objects, dropped once the sessions are gone
}

var seq atomic.Int64

// New creates the workload's two tables; sessions are started separately.
func New(t testing.TB, dsn string) *Workload {
	t.Helper()
	n := fmt.Sprintf("%d_%d", time.Now().UnixNano()%1_000_000_000, seq.Add(1))
	w := &Workload{DSN: dsn, AppTable: "public.selfload_app_" + n,
		SageTable: "public.selfload_sage_" + n, suffix: n,
		admin: connect(t, dsn, "selfload_admin", false)}
	t.Cleanup(func() {
		defer func() { _ = w.admin.Close(context.Background()) }()
		w.awaitGone(t)
		for _, drop := range append(w.drops,
			"DROP TABLE IF EXISTS "+w.AppTable+", "+w.SageTable) {
			if _, err := w.admin.Exec(context.Background(), drop); err != nil {
				t.Errorf("selfload: %s: %v", drop, err)
			}
		}
	})
	for _, table := range []string{w.AppTable, w.SageTable} {
		if _, err := w.admin.Exec(context.Background(),
			"CREATE TABLE "+table+" (id int); INSERT INTO "+table+" VALUES (1)"); err != nil {
			t.Fatalf("selfload: create %s: %v", table, err)
		}
	}
	return w
}

// Start creates a workload and starts the application group, then the
// pg_sage group.
func Start(t testing.TB, dsn string) (*Workload, *Group, *Group) {
	t.Helper()
	w := New(t, dsn)
	app := w.StartApp(t)
	return w, app, w.StartSage(t, 0)
}

// StartApp starts the application group.
func (w *Workload) StartApp(t testing.TB) *Group {
	t.Helper()
	return w.start(t, false, w.AppTable, 0)
}

// StartSage starts the pg_sage group with extra additional sleepers.
// The application group must be running (its holder blocks the waiter).
func (w *Workload) StartSage(t testing.TB, extra int) *Group {
	t.Helper()
	return w.start(t, true, w.SageTable, extra)
}

func (w *Workload) start(t testing.TB, sage bool, own string, extra int) *Group {
	t.Helper()
	g := &Group{}
	holder := connect(t, w.DSN, AppName, sage)
	g.Holder = pidOf(t, holder)
	w.pids = append(w.pids, g.Holder)
	if _, err := holder.Exec(context.Background(), "BEGIN"); err != nil {
		t.Fatalf("selfload: begin: %v", err)
	}
	if _, err := holder.Exec(context.Background(),
		"LOCK TABLE "+own+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("selfload: lock %s: %v", own, err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	g.Waiter = w.background(t, sage, "SELECT count(*) AS selfload_wait FROM "+w.AppTable)
	g.Sleeper = w.background(t, sage, "SELECT pg_sleep(600) AS selfload_sleep")
	for range extra {
		g.Extra = append(g.Extra, w.background(t, sage,
			"SELECT pg_sleep(600) AS selfload_sleep"))
	}
	w.awaitStates(t, g)
	return g
}

// background runs sql on a new session until the test ends.
func (w *Workload) background(t testing.TB, sage bool, sql string) int {
	t.Helper()
	conn := connect(t, w.DSN, AppName, sage)
	pid := pidOf(t, conn)
	w.pids = append(w.pids, pid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.Exec(ctx, sql)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = conn.Close(context.Background())
	})
	return pid
}

// awaitStates waits until every session of g is in its state.
func (w *Workload) awaitStates(t testing.TB, g *Group) {
	t.Helper()
	want := map[int]string{g.Holder: "idle in transaction", g.Waiter: "Lock",
		g.Sleeper: "PgSleep"}
	for _, pid := range g.Extra {
		want[pid] = "PgSleep"
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		missing := w.missingStates(t, want)
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("selfload: sessions not ready: %s", strings.Join(missing, ", "))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitGone ends every session the workload started. Closing a client
// does not wait for its backend: an aborting transaction can outlive the
// close briefly, and a pg_sleep whose cancel request was lost runs on
// until it ends, since a sleeping backend does not see its client go. The
// next test of the package counts this database's sessions exactly, so
// sessions still there after a grace period are terminated.
func (w *Workload) awaitGone(t testing.TB) {
	t.Helper()
	if w.waitGone(t, 2*time.Second) == 0 {
		return
	}
	if _, err := w.admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity WHERE pid = ANY($1)`, w.pids); err != nil {
		t.Errorf("selfload: terminate the workload's sessions: %v", err)
		return
	}
	if left := w.waitGone(t, 15*time.Second); left > 0 {
		t.Errorf("selfload: %d of the workload's sessions still running after "+
			"pg_terminate_backend", left)
	}
}

// waitGone waits up to timeout for the workload's sessions to leave
// pg_stat_activity and returns how many are left.
func (w *Workload) waitGone(t testing.TB, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var left int
		err := w.admin.QueryRow(context.Background(), `SELECT count(*)
			FROM pg_stat_activity WHERE pid = ANY($1)`, w.pids).Scan(&left)
		if err != nil {
			t.Errorf("selfload: read remaining sessions: %v", err)
			return 0
		}
		if left == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (w *Workload) missingStates(t testing.TB, want map[int]string) []string {
	t.Helper()
	var missing []string
	for pid, state := range want {
		var ok bool
		err := w.admin.QueryRow(context.Background(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE pid = $1
			   AND (state = $2 OR (state = 'active' AND wait_event_type = $2)
			        OR (state = 'active' AND wait_event = $2)))`, pid, state).Scan(&ok)
		if err != nil {
			t.Fatalf("selfload: read session state: %v", err)
		}
		if !ok {
			missing = append(missing, fmt.Sprintf("%d %s", pid, state))
		}
	}
	return missing
}

// connect opens one session; a pg_sage session is configured exactly as
// pg_sage's pools are (selfmonitor.ConfigurePool).
func connect(t testing.TB, dsn, app string, sage bool) *pgx.Conn {
	t.Helper()
	cfg := SagePoolConfig(t, dsn)
	conf := cfg.ConnConfig
	if !sage {
		var err error
		if conf, err = pgx.ParseConfig(dsn); err != nil {
			t.Fatalf("selfload: parse DSN: %v", err)
		}
		conf.RuntimeParams["application_name"] = app
	}
	conn, err := pgx.ConnectConfig(context.Background(), conf)
	if err != nil {
		t.Fatalf("selfload: connect: %v", err)
	}
	return conn
}

// SagePoolConfig is dsn configured like pg_sage's own pools.
func SagePoolConfig(t testing.TB, dsn string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("selfload: parse DSN: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	return cfg
}

// SagePool opens a pool configured like pg_sage's own pools.
func SagePool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), SagePoolConfig(t, dsn))
	if err != nil {
		t.Fatalf("selfload: pg_sage pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func pidOf(t testing.TB, conn *pgx.Conn) int {
	t.Helper()
	return int(conn.PgConn().PID())
}

// MinOver returns the smallest of n samples of read, taken 25 ms apart.
// Concurrent test packages only ever add sessions to a cluster-wide
// count, so the minimum is the closest sample to the test's own load.
func MinOver(t testing.TB, n int, read func() (float64, error)) float64 {
	t.Helper()
	low := 0.0
	for i := range n {
		v, err := read()
		if err != nil {
			t.Fatalf("selfload: sample: %v", err)
		}
		if i == 0 || v < low {
			low = v
		}
		time.Sleep(25 * time.Millisecond)
	}
	return low
}
