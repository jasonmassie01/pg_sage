package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// vacuum-11c: the freeze sampler's one read-only transaction is bounded by
// the safety statement and lock timeouts, set before any catalog read.
// No concurrent-access test here: the shared-reading test in
// freeze_sampler_test.go asserts one bounded transaction for parallel readers.

func lockTimeoutErr() error {
	return &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
}

func statementTimeoutErr() error {
	return &pgconn.PgError{Code: "57014",
		Message: "canceling statement due to statement timeout"}
}

// boundArgs returns the statement and lock timeouts the reading's first
// statement set, failing the test unless that statement is the bounds.
func boundArgs(t *testing.T, db *fakeSamplerDB) (string, string) {
	t.Helper()
	stmts := db.statements()
	if len(stmts) == 0 {
		t.Fatal("the reading ran no statement")
	}
	first := stmts[0]
	if first.method != "Exec" || !strings.Contains(first.sql,
		"set_config('statement_timeout', $1, true)") ||
		!strings.Contains(first.sql, "set_config('lock_timeout', $2, true)") {
		t.Fatalf("first statement = %s %q, want the bounds' set_config", first.method, first.sql)
	}
	if len(first.args) != 2 {
		t.Fatalf("set_config args = %#v, want the two timeouts", first.args)
	}
	return fmt.Sprint(first.args[0]), fmt.Sprint(first.args[1])
}

func custodianOver(db *fakeSamplerDB) *PostgresFreezeCustodian {
	c := NewPostgresFreezeCustodian(nil, "db", 25)
	c.sampler.pool = db
	return c
}

func TestFreezeSamplerBoundsItsReadWithTheConfiguredTimeouts(t *testing.T) {
	db := &fakeSamplerDB{}
	c := custodianOver(db).WithBloatBounds(0,
		catalogread.Timeouts{Statement: 5 * time.Second, Lock: 2 * time.Second})

	if _, err := c.Sampler().Read(context.Background()); err != nil {
		t.Fatalf("Read: %v", err)
	}

	stmt, lock := boundArgs(t, db)
	if stmt != "5000ms" || lock != "2000ms" {
		t.Fatalf("bounds = %s, %s; want 5000ms, 2000ms", stmt, lock)
	}
	if n := len(db.statements()); n != 4 {
		t.Fatalf("reading ran %d statements, want bounds, counters, holders, horizons", n)
	}
}

func TestFreezeSamplerDefaultsToTheShippedBounds(t *testing.T) {
	db := &fakeSamplerDB{}
	if _, err := NewFreezeSampler(db, 0, 0).Read(context.Background()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	def := catalogread.Default()
	stmt, lock := boundArgs(t, db)
	want := [2]string{fmt.Sprintf("%dms", def.Statement.Milliseconds()),
		fmt.Sprintf("%dms", def.Lock.Milliseconds())}
	if [2]string{stmt, lock} != want || stmt == "0ms" || lock == "0ms" {
		t.Fatalf("bounds = %s, %s; want the shipped %v", stmt, lock, want)
	}
}

// Boundary and invalid input: a zero or negative timeout never reaches the
// sampler as 0ms (no limit); the shipped value fills it.
func TestFreezeSamplerZeroOrNegativeBoundsKeepTheDefaults(t *testing.T) {
	def := catalogread.Default()
	cases := []struct {
		name string
		in   catalogread.Timeouts
		want catalogread.Timeouts
	}{
		{"zero", catalogread.Timeouts{}, def},
		{"negative", catalogread.Timeouts{Statement: -time.Second, Lock: -time.Millisecond}, def},
		{"zero lock", catalogread.Timeouts{Statement: 7 * time.Second},
			catalogread.Timeouts{Statement: 7 * time.Second, Lock: def.Lock}},
		{"negative statement", catalogread.Timeouts{Statement: -1, Lock: time.Millisecond},
			catalogread.Timeouts{Statement: def.Statement, Lock: time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewPostgresFreezeCustodian(nil, "db", 25).WithBloatBounds(0, tc.in)
			if got := c.Sampler().bounds; got != tc.want || got.Statement <= 0 || got.Lock <= 0 {
				t.Fatalf("sampler bounds = %+v, want %+v", got, tc.want)
			}
			if c.confirmBounds != c.Sampler().bounds {
				t.Fatalf("confirm bounds %+v differ from the sampler's %+v", c.confirmBounds,
					c.Sampler().bounds)
			}
		})
	}
}

// Error propagation: bounds that cannot be set fail the reading before any
// catalog read runs.
func TestFreezeSamplerBoundsFailureReadsNothing(t *testing.T) {
	db := &fakeSamplerDB{execErr: errors.New("permission denied for set_config")}
	_, err := NewFreezeSampler(db, 0, 0).Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "begin freeze age read") ||
		!strings.Contains(err.Error(), "permission denied for set_config") {
		t.Fatalf("Read error = %v, want the wrapped bounds failure", err)
	}
	if n := len(db.statements()); n != 1 {
		t.Fatalf("reading ran %d statements after the bounds failed, want only them", n)
	}
}

func TestFreezeSamplerTimeoutsNameTheBound(t *testing.T) {
	cases := []struct {
		name, want string
		err        error
	}{
		{"lock", "freeze age read: lock timeout", lockTimeoutErr()},
		{"statement", "freeze age read: statement timeout", statementTimeoutErr()},
		{"other", "freeze age read: read freeze horizons: relation vanished",
			errors.New("relation vanished")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeSamplerDB{horizonErr: tc.err}
			_, err := NewFreezeSampler(db, 0, 0).Read(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Read error = %v, want it to contain %q", err, tc.want)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("Read error %v does not wrap the driver's %v", err, tc.err)
			}
		})
	}
}

// A user cancel shares SQLSTATE 57014 with a statement timeout; it is not
// reported as one.
func TestFreezeSamplerCancelIsNotCalledAStatementTimeout(t *testing.T) {
	cancel := &pgconn.PgError{Code: "57014",
		Message: "canceling statement due to user request"}
	db := &fakeSamplerDB{horizonErr: cancel}
	_, err := NewFreezeSampler(db, 0, 0).Read(context.Background())
	if err == nil || strings.Contains(err.Error(), "statement timeout") ||
		!strings.Contains(err.Error(), "freeze age read: read freeze horizons") {
		t.Fatalf("Read error = %v, want a plain freeze age read error", err)
	}
}

// State transitions: a timed-out reading after the TTL returns the error,
// never the stale reading; the next reading advances the rates from the
// last good counters.
func TestFreezeSamplerTimedOutReadThenRecovers(t *testing.T) {
	db := &fakeSamplerDB{}
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	sampler := NewFreezeSampler(db, 0, 5*time.Second)
	sampler.now = clock.now
	first, err := sampler.Read(context.Background())
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	clock.mu.Lock()
	clock.at = clock.at.Add(10 * time.Second)
	clock.mu.Unlock()
	db.horizonErr = lockTimeoutErr()

	stale, err := sampler.Read(context.Background())

	if err == nil || !strings.Contains(err.Error(), "lock timeout") || !stale.At.IsZero() {
		t.Fatalf("timed-out Read = %v at %v, want the lock timeout and no reading", err, stale.At)
	}
	db.horizonErr = nil
	fresh, err := sampler.Read(context.Background())
	if err != nil || !fresh.rates.known || db.begins.Load() != 3 {
		t.Fatalf("recovered Read: %d readings, rates %+v (%v)", db.begins.Load(),
			fresh.rates, err)
	}
	// 2,000 XIDs (the failed reading's counters were never kept) since the
	// first good reading.
	want := 2000 / fresh.At.Sub(first.At).Seconds()
	if fresh.rates.xid < want*0.99 || fresh.rates.xid > want*1.01 {
		t.Fatalf("xid rate = %v, want about %v (against the first reading)", fresh.rates.xid,
			want)
	}
}
