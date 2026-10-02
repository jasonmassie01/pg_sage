package pooler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The pooler telemetry probe (CHECK-04) reads PgBouncer's admin console
// with SHOW POOLS and SHOW STATS only: bounded in time and rows,
// read-only, and never exposing the DSN or its credentials. A pooler
// that cannot be read is reported as such, never as an idle pool.

const secretDSN = "postgres://stats:hunter2-SECRET@pgb.internal:6432/pgbouncer"

type fakeAdmin struct {
	pools, stats []map[string]any
	poolsErr     error
	statsErr     error
	block        bool // Show blocks until ctx ends
	mu           sync.Mutex
	shows        []string
	closed       bool
}

func (f *fakeAdmin) Show(ctx context.Context, what string) ([]map[string]any, error) {
	f.mu.Lock()
	f.shows = append(f.shows, what)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	switch what {
	case ShowPools:
		return f.pools, f.poolsErr
	case ShowStats:
		return f.stats, f.statsErr
	}
	return nil, fmt.Errorf("unexpected SHOW %s", what)
}

func (f *fakeAdmin) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func poolRow(db string, waiting, maxwait, maxwaitUS int64) map[string]any {
	return map[string]any{"database": db, "user": "app", "cl_active": int64(20),
		"cl_waiting": waiting, "sv_active": int64(20), "sv_idle": int64(0),
		"sv_used": int64(0), "sv_tested": int64(0), "sv_login": int64(0),
		"maxwait": maxwait, "maxwait_us": maxwaitUS, "pool_mode": "transaction"}
}

func dialerOf(admins map[string]*fakeAdmin, errs map[string]error) Dialer {
	return func(_ context.Context, dsn string) (Admin, error) {
		if err := errs[dsn]; err != nil {
			return nil, err
		}
		a, ok := admins[dsn]
		if !ok {
			return nil, errors.New("no such pooler")
		}
		return a, nil
	}
}

func newSource(t *testing.T, d Dialer, cfgs ...Config) *Source {
	t.Helper()
	s, err := New(cfgs, d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestProbe_ReadsPoolsAndStats(t *testing.T) {
	a := &fakeAdmin{
		pools: []map[string]any{poolRow("orders", 7, 2, 500000), poolRow("reports", 0, 0, 0),
			poolRow("pgbouncer", 0, 0, 0)},
		stats: []map[string]any{{"database": "orders", "avg_wait_time": int64(1850000)},
			{"database": "reports", "avg_wait_time": int64(0)}},
	}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"dsn-1": a}, nil),
		Config{Name: "pgb-1", DSN: "dsn-1", Timeout: time.Second})
	res := s.Probe(context.Background(), probes.Args{})
	if res.Status != probes.StatusOK || res.ProbeID != probes.PoolerPools ||
		res.Version == "" || res.ObservedAt.IsZero() {
		t.Fatalf("result = %+v", res)
	}
	pools, failures, err := probes.PoolerPoolsOf(res)
	if err != nil || len(failures) != 0 || len(pools) != 2 {
		t.Fatalf("pools %+v failures %+v (%v): the admin console must be left out",
			pools, failures, err)
	}
	p := pools[0]
	if p.Pooler != "pgb-1" || p.Database != "orders" || p.ClientWaiting != 7 ||
		p.ServerIdle != 0 || p.MaxWaitS != 2.5 || p.AvgWaitUS != 1850000 ||
		p.PoolMode != "transaction" {
		t.Fatalf("orders pool = %+v", p)
	}
	if !a.closed {
		t.Fatal("the admin connection was left open")
	}
	for _, sh := range a.shows {
		if sh != ShowPools && sh != ShowStats {
			t.Fatalf("ran SHOW %s; only POOLS and STATS are allowed", sh)
		}
	}
}

// pools narrows the pools read to the named PgBouncer databases.
func TestProbe_PoolFilter(t *testing.T) {
	a := &fakeAdmin{pools: []map[string]any{poolRow("orders", 1, 0, 0),
		poolRow("reports", 9, 9, 0)}}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"d": a}, nil),
		Config{Name: "pgb-1", DSN: "d", Pools: []string{"orders"}, Timeout: time.Second})
	pools, _, err := probes.PoolerPoolsOf(s.Probe(context.Background(), probes.Args{}))
	if err != nil || len(pools) != 1 || pools[0].Database != "orders" {
		t.Fatalf("filtered pools = %+v (%v)", pools, err)
	}
}

// PgBouncer before 1.8 has no maxwait_us and older SHOW STATS has no
// avg_wait_time: the missing numbers are unknown, not zero.
func TestProbe_OlderPgBouncerColumns(t *testing.T) {
	row := poolRow("orders", 3, 4, 0)
	delete(row, "maxwait_us")
	a := &fakeAdmin{pools: []map[string]any{row},
		stats: []map[string]any{{"database": "orders"}}}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"d": a}, nil),
		Config{Name: "pgb-1", DSN: "d", Timeout: time.Second})
	pools, _, err := probes.PoolerPoolsOf(s.Probe(context.Background(), probes.Args{}))
	if err != nil || len(pools) != 1 || pools[0].MaxWaitS != 4 ||
		pools[0].AvgWaitUS == pools[0].AvgWaitUS { // NaN != NaN
		t.Fatalf("pools = %+v (%v)", pools, err)
	}
}

// SHOW STATS failing still reports the pools (its numbers unknown).
func TestProbe_StatsFailureKeepsThePools(t *testing.T) {
	a := &fakeAdmin{pools: []map[string]any{poolRow("orders", 3, 1, 0)},
		statsErr: errors.New("stats broke")}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"d": a}, nil),
		Config{Name: "pgb-1", DSN: "d", Timeout: time.Second})
	res := s.Probe(context.Background(), probes.Args{})
	pools, _, err := probes.PoolerPoolsOf(res)
	if res.Status != probes.StatusOK || err != nil || len(pools) != 1 ||
		pools[0].ClientWaiting != 3 {
		t.Fatalf("result %+v pools %+v (%v)", res, pools, err)
	}
}

func TestProbe_FailuresAreClassifiedAndNeverLeakTheDSN(t *testing.T) {
	auth := &pgconn.PgError{Code: "28P01", Message: "password authentication failed " +
		"for user \"stats\" (" + secretDSN + ")"}
	cases := map[string]struct {
		err    error
		status probes.Status
		reason string
	}{
		"refused": {fmt.Errorf("dial %s: connection refused", secretDSN),
			probes.StatusError, ReasonUnreachable},
		"auth":     {auth, probes.StatusNoPrivilege, ReasonAuthFailed},
		"deadline": {context.DeadlineExceeded, probes.StatusError, ReasonTimeout},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSource(t, dialerOf(nil, map[string]error{secretDSN: c.err}),
				Config{Name: "pgb-1", DSN: secretDSN, Timeout: time.Second})
			res := s.Probe(context.Background(), probes.Args{})
			if res.Status != c.status || res.Reason != c.reason {
				t.Fatalf("result = %+v, want %s/%s", res, c.status, c.reason)
			}
			raw, _ := res.Payload()
			for _, secret := range []string{"hunter2", "SECRET", "pgb.internal", "stats:"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("result leaks %q: %s", secret, raw)
				}
			}
		})
	}
}

// One pooler of two failing: the reachable one's pools are reported and
// the other is a failure row, so the result is still usable.
func TestProbe_PartialFailure(t *testing.T) {
	a := &fakeAdmin{pools: []map[string]any{poolRow("orders", 5, 2, 0)}}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"ok": a},
		map[string]error{"down": errors.New("connection refused")}),
		Config{Name: "pgb-1", DSN: "ok", Timeout: time.Second},
		Config{Name: "pgb-2", DSN: "down", Timeout: time.Second})
	res := s.Probe(context.Background(), probes.Args{})
	pools, failures, err := probes.PoolerPoolsOf(res)
	if res.Status != probes.StatusOK || err != nil || len(pools) != 1 ||
		len(failures) != 1 || failures[0] != (probes.PoolerFailure{Pooler: "pgb-2",
		Reason: ReasonUnreachable}) {
		t.Fatalf("result %+v pools %+v failures %+v (%v)", res, pools, failures, err)
	}
}

// A pooler that hangs is cut off at its timeout, and the probe returns
// in bounded time.
func TestProbe_TimeoutIsBounded(t *testing.T) {
	a := &fakeAdmin{block: true}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"d": a}, nil),
		Config{Name: "pgb-1", DSN: "d", Timeout: 100 * time.Millisecond})
	start := time.Now()
	res := s.Probe(context.Background(), probes.Args{})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a hung pooler held the probe for %s", elapsed)
	}
	if res.Status != probes.StatusError || res.Reason != ReasonTimeout {
		t.Fatalf("hung pooler = %+v, want %s", res, ReasonTimeout)
	}
	if !a.closed {
		t.Fatal("the hung admin connection was not closed")
	}
}

// Rows are capped at the probe ceiling; the cut is flagged.
func TestProbe_RowCap(t *testing.T) {
	var rows []map[string]any
	for i := 0; i < probes.MaxRows+50; i++ {
		rows = append(rows, poolRow(fmt.Sprintf("db%03d", i), 0, 0, 0))
	}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"d": {pools: rows}}, nil),
		Config{Name: "pgb-1", DSN: "d", Timeout: time.Second})
	res := s.Probe(context.Background(), probes.Args{})
	if len(res.Rows) != probes.MaxRows || !res.Truncated {
		t.Fatalf("%d rows truncated=%v, want %d and truncated", len(res.Rows),
			res.Truncated, probes.MaxRows)
	}
}

func TestProbe_InvalidCalls(t *testing.T) {
	var nilSource *Source
	if res := nilSource.Probe(context.Background(), probes.Args{}); res.Status !=
		probes.StatusError || res.Reason != "not_configured" {
		t.Fatalf("nil source = %+v", res)
	}
	s := newSource(t, dialerOf(map[string]*fakeAdmin{"d": {}}, nil),
		Config{Name: "pgb-1", DSN: "d", Timeout: time.Second})
	if res := s.Probe(context.Background(), probes.Args{PID: 5}); res.Status !=
		probes.StatusError || res.Reason != "invalid_args" {
		t.Fatalf("arguments = %+v, want invalid_args", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := s.Probe(ctx, probes.Args{}); res.Status != probes.StatusError {
		t.Fatalf("canceled = %+v", res)
	}
	// An empty pool list (only the admin console) is an observed absence.
	empty := newSource(t, dialerOf(map[string]*fakeAdmin{"d": {pools: []map[string]any{
		poolRow("pgbouncer", 0, 0, 0)}}}, nil), Config{Name: "pgb-1", DSN: "d",
		Timeout: time.Second})
	if res := empty.Probe(context.Background(), probes.Args{}); res.Status !=
		probes.StatusEmpty {
		t.Fatalf("no pools = %+v, want empty", res)
	}
}

func TestNew_Validates(t *testing.T) {
	d := dialerOf(nil, nil)
	good := Config{Name: "pgb-1", DSN: "d", Timeout: time.Second}
	cases := map[string][]Config{
		"none":         nil,
		"no name":      {{DSN: "d", Timeout: time.Second}},
		"bad name":     {{Name: "PGB 1;", DSN: "d", Timeout: time.Second}},
		"no dsn":       {{Name: "pgb-1", Timeout: time.Second}},
		"duplicate":    {good, good},
		"zero timeout": {{Name: "pgb-1", DSN: "d"}},
		"long timeout": {{Name: "pgb-1", DSN: "d", Timeout: time.Minute}},
		"too many pools": {{Name: "pgb-1", DSN: "d", Timeout: time.Second,
			Pools: make([]string, 101)}},
	}
	for name, cfgs := range cases {
		if _, err := New(cfgs, d); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error leaks the DSN: %v", name, err)
		}
	}
	if _, err := New([]Config{good}, nil); err == nil {
		t.Error("a nil dialer was accepted")
	}
	if _, err := New([]Config{{Name: "pgb-1", DSN: secretDSN, Timeout: time.Minute}},
		d); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("invalid config error = %v (must not leak the DSN)", err)
	}
}

// Concurrent probes share no mutable state (run with -race).
func TestProbe_Concurrent(t *testing.T) {
	d := func(context.Context, string) (Admin, error) {
		return &fakeAdmin{pools: []map[string]any{poolRow("orders", 2, 1, 0)}}, nil
	}
	s := newSource(t, d, Config{Name: "pgb-1", DSN: "d", Timeout: time.Second})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := s.Probe(context.Background(), probes.Args{}); res.Status !=
				probes.StatusOK {
				t.Errorf("concurrent probe = %+v", res)
			}
		}()
	}
	wg.Wait()
}
