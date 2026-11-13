package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// sre-12: lease budgets follow the database clock's remaining time, not
// the database's absolute timestamps read on the host clock. A skewed
// database clock is simulated by shadowing clock_timestamp() on the
// store's own connections (a schema ahead of pg_catalog in search_path),
// so every lease write and guard sees one consistently skewed clock.

var clockSkews = []time.Duration{0, 5 * time.Second, -5 * time.Second}

// skewedStore is a store whose database clock runs skew ahead of the host.
func skewedStore(t *testing.T, limits Limits, skew time.Duration) (*PostgresStore,
	*pgxpool.Pool, context.Context) {
	t.Helper()
	_, admin, ctx := liveStore(t, limits) // bootstraps the schema
	schemaName := "sre_skew_" + strings.ReplaceAll(string(NewUUID()), "-", "")
	ident := pgx.Identifier{schemaName}.Sanitize()
	for _, ddl := range []string{"CREATE SCHEMA " + ident, fmt.Sprintf(`CREATE FUNCTION
		%s.clock_timestamp() RETURNS timestamptz LANGUAGE sql VOLATILE
		AS $$ SELECT pg_catalog.clock_timestamp() + make_interval(secs => %f) $$`,
		ident, skew.Seconds())} {
		if _, err := admin.Exec(ctx, ddl); err != nil {
			t.Fatalf("create skewed clock: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
	})
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schemaName + ", pg_catalog, public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect skewed: %v", err)
	}
	t.Cleanup(pool.Close)
	var got float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM clock_timestamp()
		- pg_catalog.clock_timestamp())::float8`).Scan(&got); err != nil ||
		got < skew.Seconds()-1 || got > skew.Seconds()+1 {
		t.Fatalf("skewed clock reads %.3f s ahead (%v), want %v", got, err, skew)
	}
	st, err := NewPostgresStore(pool, limits)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	return st, pool, ctx
}

// dbLeft reads the lease's remaining times on the (skewed) database clock.
func dbLeft(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id UUID) (until, segment time.Duration) {
	t.Helper()
	var u, s float64
	if err := pool.QueryRow(ctx, `SELECT
		EXTRACT(EPOCH FROM lease_until - clk.ts)::float8,
		EXTRACT(EPOCH FROM segment_deadline - clk.ts)::float8
		FROM sage.sre_investigations, (SELECT clock_timestamp() AS ts) clk
		WHERE id = $1`, string(id)).Scan(&u, &s); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	return time.Duration(u * float64(time.Second)), time.Duration(s * float64(time.Second))
}

// assertFollowsDatabase checks the happy-path bound: measured after the
// remaining was read, the host's time left is at most the database's and
// short of it by no more than the time spent in the claim and the read.
func assertFollowsDatabase(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	l Lease, began time.Time) {
	t.Helper()
	until, segment := dbLeft(t, ctx, pool, l.InvestigationID)
	hostUntil, hostSegment := time.Until(l.Until), time.Until(l.SegmentDeadline)
	slack := time.Since(began)
	for _, c := range []struct {
		name       string
		host, want time.Duration
	}{{"lease", hostUntil, until}, {"segment", hostSegment, segment}} {
		if c.host > c.want || c.host < c.want-slack {
			t.Errorf("%s time left on the host = %v, want in (%v, %v] (database "+
				"remaining, minus %v spent querying)", c.name, c.host, c.want-slack, c.want,
				slack)
		}
	}
	if l.Until.After(l.SegmentDeadline) {
		t.Errorf("lease until %v is after its segment deadline %v", l.Until,
			l.SegmentDeadline)
	}
}

func TestPostgresLeaseClock_ClaimFollowsTheDatabaseRemaining(t *testing.T) {
	for _, skew := range clockSkews {
		t.Run(fmt.Sprintf("skew=%v", skew), func(t *testing.T) {
			st, pool, ctx := skewedStore(t, DefaultLimits(), skew)
			inv, _, err := st.Create(ctx, lockStart(testScope(t, ctx, st), "clock-claim"))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			began := time.Now()
			lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			assertFollowsDatabase(t, ctx, pool, lease, began)
			ttl := DefaultLimits().LeaseTTL
			if left := time.Until(lease.Until); left > ttl || left < ttl-5*time.Second {
				t.Fatalf("a fresh lease has %v left, want at most the TTL %v", left, ttl)
			}
		})
	}
}

func TestPostgresLeaseClock_HeartbeatOnALostLeaseHasNoDeadlines(t *testing.T) {
	for _, skew := range clockSkews {
		t.Run(fmt.Sprintf("skew=%v", skew), func(t *testing.T) {
			st, _, ctx := skewedStore(t, DefaultLimits(), skew)
			inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "clock-lost"))
			lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			stale := lease
			stale.Fence++
			got, err := st.Heartbeat(ctx, stale)
			if !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("heartbeat on a lost lease = %v, want ErrLeaseLost", err)
			}
			if !got.Until.IsZero() || !got.SegmentDeadline.IsZero() || got.Fence != 0 {
				t.Fatalf("lost heartbeat returned deadlines %+v, want a zero lease", got)
			}
		})
	}
}

// Two workers race for one investigation under a skewed clock; the
// winner's deadlines follow the database, the loser gets no lease.
func TestPostgresLeaseClock_RacingClaimsUnderSkew(t *testing.T) {
	st, pool, ctx := skewedStore(t, DefaultLimits(), 5*time.Second)
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "clock-race"))
	began := time.Now()
	var wg sync.WaitGroup
	leases := make([]Lease, 2)
	errs := make([]error, 2)
	for i := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leases[i], errs[i] = st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
		}()
	}
	wg.Wait()
	winner, loser := 0, 1
	if errs[0] != nil {
		winner, loser = 1, 0
	}
	if errs[winner] != nil || !errors.Is(errs[loser], ErrLeaseUnavailable) {
		t.Fatalf("claims = %v, %v; want one lease and one ErrLeaseUnavailable", errs[0],
			errs[1])
	}
	if !leases[loser].Until.IsZero() || !leases[loser].SegmentDeadline.IsZero() {
		t.Fatalf("losing claim carries deadlines: %+v", leases[loser])
	}
	assertFollowsDatabase(t, ctx, pool, leases[winner], began)
}

// claim -> heartbeat (moves forward by at most the TTL) -> segment end
// (the heartbeat cannot pass the segment) -> release (no more heartbeats).
func TestPostgresLeaseClock_LeaseLifecycleUnderSkew(t *testing.T) {
	for _, skew := range clockSkews {
		t.Run(fmt.Sprintf("skew=%v", skew), func(t *testing.T) {
			st, pool, ctx := skewedStore(t, DefaultLimits(), skew)
			ttl := DefaultLimits().LeaseTTL
			inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "clock-life"))
			a, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			began := time.Now()
			b, err := st.Heartbeat(ctx, a)
			if err != nil || !b.Until.After(a.Until) || time.Until(b.Until) > ttl {
				t.Fatalf("heartbeat = %v (%v) after %v, want later and at most %v left",
					b.Until, err, a.Until, ttl)
			}
			assertFollowsDatabase(t, ctx, pool, b, began)
			heartbeatAtSegmentEnd(t, ctx, st, pool, b)
			if _, err := st.CommitStep(ctx, b, step("eval", StateEvaluating)); err != nil {
				t.Fatalf("collecting -> evaluating: %v", err)
			}
			if _, err := st.Release(ctx, b, StateNeedsEvidence); err != nil {
				t.Fatalf("release: %v", err)
			}
			if _, err := st.Heartbeat(ctx, b); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("heartbeat after release = %v, want ErrLeaseLost", err)
			}
		})
	}
}

// heartbeatAtSegmentEnd moves the segment end to one second away on the
// database clock; the heartbeat is capped there, on the host clock too.
func heartbeatAtSegmentEnd(t *testing.T, ctx context.Context, st *PostgresStore,
	pool *pgxpool.Pool, l Lease) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET segment_deadline = clock_timestamp() + interval '1 second',
		    lease_until = clock_timestamp() + interval '1 second'
		WHERE id = $1`, string(l.InvestigationID)); err != nil {
		t.Fatalf("shorten segment: %v", err)
	}
	began := time.Now()
	c, err := st.Heartbeat(ctx, l)
	if err != nil {
		t.Fatalf("heartbeat at segment end: %v", err)
	}
	if !c.Until.Equal(c.SegmentDeadline) || time.Until(c.SegmentDeadline) > time.Second {
		t.Fatalf("heartbeat at segment end: until=%v segment=%v (%v left), want both "+
			"at the segment end, at most 1s away", c.Until, c.SegmentDeadline,
			time.Until(c.SegmentDeadline))
	}
	assertFollowsDatabase(t, ctx, pool, c, began)
}

// Boundary: a claim with one millisecond of active time left gets a
// deadline at most that far away, whatever the skew.
func TestPostgresLeaseClock_ClaimAtTheBudgetEdge(t *testing.T) {
	for _, skew := range clockSkews {
		t.Run(fmt.Sprintf("skew=%v", skew), func(t *testing.T) {
			limits := DefaultLimits()
			st, pool, ctx := skewedStore(t, limits, skew)
			inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "clock-edge"))
			if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations SET active_ms = $2
				WHERE id = $1`, string(inv.ID), limits.MaxActive.Milliseconds()-1); err != nil {
				t.Fatalf("spend budget: %v", err)
			}
			lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
			if err != nil {
				t.Fatalf("claim with 1 ms left: %v", err)
			}
			if left := time.Until(lease.SegmentDeadline); left > time.Millisecond {
				t.Fatalf("1 ms of budget gave %v of segment on the host", left)
			}
			if left := time.Until(lease.Until); left > time.Millisecond {
				t.Fatalf("1 ms of budget gave %v of lease on the host", left)
			}
		})
	}
}
