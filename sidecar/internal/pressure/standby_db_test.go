package pressure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// guard-01a CHECK-40: on a local primary and streaming standby pair, a
// backfill with replay paused reads stop while the replay lag is still
// under the 30 s replication-lag alert, and proceed again within one cache
// period of the standby catching up after pg_wal_replay_resume().
//
// The standby is a second improve-pg:17 container seeded with
// pg_basebackup -R from the test server, sharing its network namespace.
// The test reaches it through dblink from the primary, so
// SAGE_TEST_PRESSURE_STANDBY_DSN is the libpq conninfo the primary uses
// for it (for example "host=127.0.0.1 port=5433 user=postgres
// dbname=postgres"). The name avoids "Pressure" so CHECK-39's
// `-run Pressure` runs never list it as skipped.

const standbyEnv = "SAGE_TEST_PRESSURE_STANDBY_DSN"

func standbyCall(t *testing.T, pool *pgxpool.Pool, dsn, fn string) {
	t.Helper()
	sql := "SELECT x FROM dblink($1, 'SELECT " + fn + "()::text') AS t(x text)"
	if _, err := pool.Exec(context.Background(), sql, dsn); err != nil {
		t.Fatalf("%s on the standby: %v", fn, err)
	}
}

func awaitStreaming(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM
			pg_stat_replication WHERE state = 'streaming'`).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_replication: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("no streaming standby within 60 s")
}

// backfillUntilStop inserts 10 MB batches (up to 200 MB) and checks the
// pressure between them; it returns the replica lag at the first stop.
func backfillUntilStop(t *testing.T, pool *pgxpool.Pool, c *Checker) float64 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(4 * time.Minute)
	for batch := 0; time.Now().Before(deadline); batch++ {
		if batch < 20 {
			exec(t, pool, "INSERT INTO pressure_backfill SELECT g, repeat('b', 1000) "+
				"FROM generate_series(1, 10000) g")
		} else {
			time.Sleep(time.Second) // 200 MB written: keep checking while lag grows
		}
		v, err := c.Check(ctx, Request{Table: "public.pressure_backfill", Job: "check-40"})
		if err != nil {
			t.Fatalf("check during the backfill: %v", err)
		}
		lag := inputOf(t, v, policy.PressureReplicaLag)
		t.Logf("batch %d: %s lag %v", batch, v.Verdict, deref(lag.Value))
		if v.Verdict == Stop {
			if lag.State != StateKnown || lag.Band != BandStop {
				t.Fatalf("stopped by %v, want the replica lag: %+v", v.Reasons, lag)
			}
			return *lag.Value
		}
	}
	t.Fatal("the backfill never read stop with replay paused")
	return 0
}

func TestReplayPausedBackfill_StopsBeforeTheAlertAndResumes(t *testing.T) {
	standby := strings.TrimSpace(os.Getenv(standbyEnv))
	if standby == "" {
		t.Skipf("%s not set: CHECK-40 needs a local streaming standby of the test server",
			standbyEnv)
	}
	pool, ctx := dbPool(t)
	exec(t, pool, "CREATE EXTENSION IF NOT EXISTS dblink")
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pressure_backfill (id int, pad text)")
	awaitStreaming(t, pool)
	const alert = 30.0
	c := NewChecker(Options{Database: "fixture", Source: NewSQLSource(pool, SQLOptions{}),
		Alerts: Alerts{ReplicaLagSeconds: alert}})
	standbyCall(t, pool, standby, "pg_wal_replay_pause")
	t.Cleanup(func() { standbyCall(t, pool, standby, "pg_wal_replay_resume") })
	if lag := backfillUntilStop(t, pool, c); lag >= alert {
		t.Fatalf("stop came at %v s of lag, at or past the %v s alert", lag, alert)
	}
	standbyCall(t, pool, standby, "pg_wal_replay_resume")
	caughtUp := awaitCaughtUp(t, pool)
	deadline := caughtUp.Add(2*time.Second + 500*time.Millisecond)
	for {
		v, err := c.Check(ctx, Request{Table: "public.pressure_backfill"})
		if err != nil {
			t.Fatalf("check after resume: %v", err)
		}
		if v.Verdict == Proceed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %s %v one cache period after the standby caught up", v.Verdict,
				v.Reasons)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// awaitCaughtUp waits for every standby to replay the primary's position
// and returns when it saw that.
func awaitCaughtUp(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var behind int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM
			pg_stat_replication WHERE replay_lsn IS NULL
			OR replay_lsn < pg_current_wal_lsn()`).Scan(&behind); err != nil {
			t.Fatalf("read replay position: %v", err)
		}
		if behind == 0 {
			return time.Now()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the standby did not catch up within 3 minutes of resuming")
	return time.Time{}
}
