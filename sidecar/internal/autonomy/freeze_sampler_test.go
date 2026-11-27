package autonomy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeSamplerDB answers a sampler reading with no holders and no
// horizons; each reading's counters advance by 1,000 XIDs.
type fakeSamplerDB struct {
	begins   atomic.Int32
	nextXID  atomic.Int64
	beginErr error
	entered  chan struct{} // closed on the first BeginTx, when set
	release  chan struct{} // BeginTx waits on it, when set
	once     sync.Once
	// execErr fails the bounds' set_config; horizonErr fails the horizon
	// read (a lock or statement timeout), when set.
	execErr    error
	horizonErr error
	mu         sync.Mutex
	stmts      []samplerStmt // every statement the readings ran, in order
}

// samplerStmt is one statement a reading ran: its method, SQL and args.
type samplerStmt struct {
	method, sql string
	args        []any
}

func (d *fakeSamplerDB) record(method, sql string, args []any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, samplerStmt{method: method, sql: sql, args: args})
}

func (d *fakeSamplerDB) statements() []samplerStmt {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]samplerStmt(nil), d.stmts...)
}

// boundCalls counts the set_config statements that bound a reading.
func (d *fakeSamplerDB) boundCalls() int {
	n := 0
	for _, s := range d.statements() {
		if s.method == "Exec" && strings.Contains(s.sql, "set_config('statement_timeout'") {
			n++
		}
	}
	return n
}

func (d *fakeSamplerDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.begins.Add(1)
	if d.entered != nil {
		d.once.Do(func() { close(d.entered) })
	}
	if d.release != nil {
		<-d.release
	}
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return &fakeSamplerTx{db: d}, nil
}

type fakeSamplerTx struct {
	pgx.Tx
	db *fakeSamplerDB
}

func (tx *fakeSamplerTx) Exec(_ context.Context, sql string, args ...any) (
	pgconn.CommandTag, error,
) {
	tx.db.record("Exec", sql, args)
	return pgconn.CommandTag{}, tx.db.execErr
}

func (tx *fakeSamplerTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	tx.db.record("QueryRow", sql, args)
	return counterRow{next: tx.db.nextXID.Add(1000)}
}

func (tx *fakeSamplerTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.db.record("Query", sql, args)
	if tx.db.horizonErr != nil && strings.Contains(sql, "freeze_horizons") {
		return nil, tx.db.horizonErr
	}
	return emptyRows{}, nil
}

func (tx *fakeSamplerTx) Commit(context.Context) error   { return nil }
func (tx *fakeSamplerTx) Rollback(context.Context) error { return nil }

type counterRow struct{ next int64 }

func (r counterRow) Scan(dest ...any) error {
	*dest[0].(*int64), *dest[1].(*int64) = r.next, 10
	return nil
}

type emptyRows struct{ pgx.Rows }

func (emptyRows) Next() bool        { return false }
func (emptyRows) Close()            {}
func (emptyRows) Err() error        { return nil }
func (emptyRows) Scan(...any) error { return errors.New("no rows") }

// stepClock advances one second per call and reports each call.
type stepClock struct {
	mu    sync.Mutex
	at    time.Time
	calls chan struct{}
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	c.at = c.at.Add(time.Second)
	at := c.at
	c.mu.Unlock()
	if c.calls != nil {
		c.calls <- struct{}{}
	}
	return at
}

// The custodian and the runway advisor read in parallel: one query runs
// and both see the same reading and rates.
func TestFreezeSamplerParallelReadersShareOneReading(t *testing.T) {
	db := &fakeSamplerDB{entered: make(chan struct{}), release: make(chan struct{})}
	clock := &stepClock{at: time.Unix(1_700_000_000, 0), calls: make(chan struct{}, 8)}
	sampler := NewFreezeSampler(db, 0, 0)
	sampler.now = clock.now
	sampler.last = freezeCounters{at: time.Unix(1_699_999_000, 0), nextXID: 0}
	var wg sync.WaitGroup
	readings := make([]FreezeReading, 2)
	errs := make([]error, 2)
	for i := range readings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readings[i], errs[i] = sampler.Read(context.Background())
		}()
	}
	<-clock.calls // both readers asked before the reading finished
	<-clock.calls
	<-db.entered
	close(db.release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}
	if n := db.begins.Load(); n != 1 {
		t.Fatalf("parallel readers ran %d readings, want 1", n)
	}
	if n := db.boundCalls(); n != 1 {
		t.Fatalf("parallel readers bounded %d transactions, want the one reading's", n)
	}
	a, b := readings[0], readings[1]
	if !a.At.Equal(b.At) || a.rates != b.rates || !a.rates.known || a.rates.xid <= 0 {
		t.Fatalf("readers saw %+v and %+v, want one known reading", a, b)
	}
}

// A coarse clock (Windows) can give the waiting reader and the reading
// the same instant: the completion counter, not the clock, makes the
// waiter share the reading.
func TestFreezeSamplerSameInstantReadersShareOneReading(t *testing.T) {
	db := &fakeSamplerDB{entered: make(chan struct{}), release: make(chan struct{})}
	at := time.Unix(1_700_000_000, 0)
	calls := make(chan struct{}, 8)
	sampler := NewFreezeSampler(db, 0, 0)
	sampler.now = func() time.Time { calls <- struct{}{}; return at }
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = sampler.Read(context.Background()) }()
	}
	<-calls // both readers read the counter and the clock first
	<-calls
	<-db.entered
	close(db.release)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("readers: %v, %v", errs[0], errs[1])
	}
	if n := db.begins.Load(); n != 1 {
		t.Fatalf("same-instant readers ran %d readings, want 1", n)
	}
}

// State transitions: a reading is served again within the TTL, and a new
// one (with rates against the last) is taken after it.
func TestFreezeSamplerTTLThenFreshReadingWithRates(t *testing.T) {
	db := &fakeSamplerDB{}
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	sampler := NewFreezeSampler(db, 0, 5*time.Second)
	sampler.now = clock.now
	first, err := sampler.Read(context.Background())
	if err != nil || first.rates.known {
		t.Fatalf("first reading = %+v, %v; want no rate yet", first.rates, err)
	}
	again, err := sampler.Read(context.Background())
	if err != nil || !again.At.Equal(first.At) || db.begins.Load() != 1 {
		t.Fatalf("within the TTL: %d readings, at %v vs %v (%v)", db.begins.Load(),
			again.At, first.At, err)
	}
	clock.mu.Lock()
	clock.at = clock.at.Add(10 * time.Second)
	clock.mu.Unlock()
	fresh, err := sampler.Read(context.Background())
	if err != nil || db.begins.Load() != 2 || !fresh.rates.known {
		t.Fatalf("after the TTL: %d readings, rates %+v (%v)", db.begins.Load(),
			fresh.rates, err)
	}
	// 1,000 XIDs over the 13 s between the two counter samples.
	if want := 1000.0 / 13; fresh.rates.xid < want*0.99 || fresh.rates.xid > want*1.01 {
		t.Fatalf("xid rate = %v, want about %v", fresh.rates.xid, want)
	}
}

func TestFreezeSamplerZeroTTLReadsEveryTime(t *testing.T) {
	db := &fakeSamplerDB{}
	sampler := NewFreezeSampler(db, 0, 0)
	for i := 0; i < 3; i++ {
		if _, err := sampler.Read(context.Background()); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if n := db.begins.Load(); n != 3 {
		t.Fatalf("sequential reads with no TTL ran %d readings, want 3", n)
	}
}

func TestFreezeSamplerErrorsAreWrappedAndNotCached(t *testing.T) {
	db := &fakeSamplerDB{beginErr: errors.New("connection refused")}
	sampler := NewFreezeSampler(db, 0, time.Hour)
	_, err := sampler.Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "begin freeze age read") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Read error = %v, want the wrapped begin error", err)
	}
	db.beginErr = nil
	if _, err := sampler.Read(context.Background()); err != nil || db.begins.Load() != 2 {
		t.Fatalf("after the failure: %d readings (%v), want a fresh one", db.begins.Load(), err)
	}
}

func TestFreezeSamplerNilPoolAndCancelledContext(t *testing.T) {
	if _, err := NewFreezeSampler(nil, 0, 0).Read(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no database pool") {
		t.Fatalf("nil pool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := &fakeSamplerDB{}
	if _, err := NewFreezeSampler(db, 0, 0).Read(ctx); !errors.Is(err, context.Canceled) ||
		db.begins.Load() != 0 {
		t.Fatalf("cancelled: %v after %d readings", err, db.begins.Load())
	}
}

// A custodian built without a pool fails its reading with an actionable
// error instead of panicking on a nil *pgxpool.Pool.
func TestFreezeSamplerOfAPoollessCustodianErrs(t *testing.T) {
	_, err := NewPostgresFreezeCustodian(nil, "db", 25).Sampler().Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no database pool") {
		t.Fatalf("pool-less custodian reading: %v, want the no-pool error", err)
	}
}

func TestFreezeSamplerNegativeTTLIsZero(t *testing.T) {
	if s := NewFreezeSampler(nil, 0, -time.Second); s.ttl != 0 {
		t.Fatalf("ttl = %v, want 0", s.ttl)
	}
	c := NewPostgresFreezeCustodian(nil, "db", 25).WithSampleTTL(-time.Minute)
	if c.Sampler().ttl != 0 {
		t.Fatalf("custodian ttl = %v, want 0", c.Sampler().ttl)
	}
}
