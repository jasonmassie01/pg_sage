package rca

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-03 on real PostgreSQL: snapshots a real collector takes (without
// the LLM advisor's configuration snapshot) drive wal_growth_spike. Steady
// load fires nothing over 20 pairs (CHECK-20), a bulk write fires within
// one interval (CHECK-21) and pg_stat_reset() fires nothing (CHECK-22).
//
// The WAL rate is the cluster's, and other test packages may write to the
// same server at the same time. The test knows how much WAL its own load
// wrote in each interval, so it judges only the pairs whose intervals are
// its load's (other writers under a quarter of it), and says so when too
// few were.

// liveCollector runs a collector every interval over pool and returns a
// function that waits for the next distinct snapshot.
func liveCollector(t *testing.T, pool *pgxpool.Pool, interval int) func() *collector.Snapshot {
	t.Helper()
	ctx := context.Background()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Collector: config.CollectorConfig{IntervalSeconds: interval, BatchSize: 100,
			MaxQueries: 50},
		Safety: config.SafetyConfig{CPUCeilingPct: 100, BackoffConsecutiveSkips: 5,
			DormantIntervalSeconds: 600},
	}
	c := collector.New(pool, cfg, version, func(string, string, ...any) {})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	var last *collector.Snapshot
	return func() *collector.Snapshot {
		deadline := time.Now().Add(time.Duration(interval)*time.Second + 60*time.Second)
		for time.Now().Before(deadline) {
			if s := c.LatestSnapshot(); s != nil && s != last {
				last = s
				return s
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("the collector took no new snapshot within its interval + 60 s")
		return nil
	}
}

// loadChunk is the bytes of one steady-load logical message.
const loadChunk = 800_000

// walLoad writes about 8 MB of WAL a second as WAL-only logical messages
// (no table pages, so no full-page images) and records when each landed.
type walLoad struct {
	mu   sync.Mutex
	at   []time.Time
	err  error
	stop func()
}

func startWALLoad(t *testing.T, pool *pgxpool.Pool) *walLoad {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &walLoad{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				l.emit(ctx, pool)
			}
		}
	}()
	l.stop = func() { cancel(); wg.Wait() }
	t.Cleanup(l.stop)
	return l
}

func (l *walLoad) emit(ctx context.Context, pool *pgxpool.Pool) {
	_, err := pool.Exec(ctx,
		`SELECT pg_logical_emit_message(false, 'rca03', repeat('x', 800000))`)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err == nil:
		l.at = append(l.at, time.Now().UTC())
	case ctx.Err() == nil && l.err == nil:
		l.err = err
	}
}

// clean reports whether the interval between two snapshots is the steady
// load's: the other WAL written in it is under a quarter of the load's.
func (l *walLoad) clean(a, b *collector.Snapshot) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ours int64
	for _, at := range l.at {
		if at.After(a.System.WALReadAt) && !at.After(b.System.WALReadAt) {
			ours += loadChunk
		}
	}
	other := b.System.WALLSN - a.System.WALLSN - ours
	return ours > 0 && other < ours/4
}

func walLivePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// WAL is cluster-wide: on the shared server other suites' writes hid the
	// steady load on every CI run, so the test skipped instead of judging.
	pool, err := pgxpool.New(context.Background(),
		testdb.Exclusive(t, "the WAL rate is measured across the whole cluster"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// walPairs walks consecutive live snapshots through one engine's WAL
// detector, as the analyzer cycle does.
type walPairs struct {
	eng          *Engine
	load         *walLoad
	next         func() *collector.Snapshot
	before, prev *collector.Snapshot
}

// step detects on the next pair; clean reports both of its intervals clean.
func (w *walPairs) step() (sig *Signal, clean bool) {
	curr := w.next()
	w.eng.mu.Lock()
	sig = w.eng.detectWALSpike(curr, w.prev, defaultCfg())
	w.eng.mu.Unlock()
	clean = w.load.clean(w.before, w.prev) && w.load.clean(w.prev, curr)
	w.before, w.prev = w.prev, curr
	return sig, clean
}

func TestWALSpikeLive_SteadyQuietBulkFiresResetQuiet(t *testing.T) {
	pool := walLivePool(t)
	next := liveCollector(t, pool, 3)
	load := startWALLoad(t, pool)
	w := &walPairs{eng: newTestEngine(), load: load, next: next}
	next() // the first snapshot may predate the load
	w.before, w.prev = next(), next()
	if w.prev.System.WALReadAt.IsZero() || w.prev.System.WALLSN <= 0 {
		t.Fatalf("no WAL position read: lsn %d at %v", w.prev.System.WALLSN,
			w.prev.System.WALReadAt)
	}
	var unjudged []string
	if n := assertQuietPairs(t, w, 20, "steady"); n < 10 { // CHECK-20
		unjudged = append(unjudged, fmt.Sprintf("steady load: only %d of 20 pairs clean", n))
	}
	if !assertBulkFires(t, w, pool) { // CHECK-21
		unjudged = append(unjudged, "bulk: the interval before it was not clean")
	}
	w.step() // the pair after the bulk compares with the bulk interval
	if _, err := pool.Exec(context.Background(), `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("pg_stat_reset: %v", err)
	}
	if n := assertQuietPairs(t, w, 3, "after pg_stat_reset"); n == 0 { // CHECK-22
		unjudged = append(unjudged, "reset: no clean pair")
	}
	load.stop()
	if load.err != nil {
		t.Fatalf("steady load: %v", load.err)
	}
	if len(unjudged) > 0 {
		t.Skipf("other writers on the shared server hid the steady load: %s",
			strings.Join(unjudged, "; "))
	}
}

// assertQuietPairs fails on a WAL spike in any clean one of n pairs and
// returns how many were clean.
func assertQuietPairs(t *testing.T, w *walPairs, n int, phase string) int {
	t.Helper()
	clean := 0
	for i := 0; i < n; i++ {
		sig, ok := w.step()
		if !ok {
			continue
		}
		clean++
		if sig != nil {
			t.Fatalf("%s pair %d fired: %+v", phase, i, sig.Metrics)
		}
	}
	return clean
}

// assertBulkFires writes about 120 MB of WAL in one statement (five times
// an interval of the steady load) and fails unless one of the two pairs
// holding it fires. It returns false when the interval before was not the
// steady load's, so the result says nothing.
func assertBulkFires(t *testing.T, w *walPairs, pool *pgxpool.Pool) bool {
	t.Helper()
	judged := w.load.clean(w.before, w.prev)
	if _, err := pool.Exec(context.Background(), `SELECT pg_logical_emit_message(false,
		'rca03', repeat('y', 10000000)) FROM generate_series(1, 12)`); err != nil {
		t.Fatalf("bulk WAL: %v", err)
	}
	fired := false
	for i := 0; i < 2 && !fired; i++ {
		sig, _ := w.step()
		fired = sig != nil
	}
	if judged && !fired {
		t.Fatal("the bulk WAL fired no wal_growth_spike within one interval")
	}
	return judged
}
