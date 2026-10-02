package causal

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-07 on real servers: a disposable PostgreSQL is restarted, and a
// disposable standby promoted, between the two samples of a connection
// and a WAL investigation. Both comparisons must be refused with the
// matching reason. The servers are not the shared test database: these
// tests run only when a disposable server is named, because they
// restart or promote it.
//
//   - SAGE_TEST_RESTARTABLE_SUPERUSER_URL: a disposable server run with a
//     restart policy (docker --restart always); the test stops it with a
//     fast shutdown of PID 1 and waits for it to come back.
//   - SAGE_TEST_CAUSAL_STANDBY_URL: a disposable streaming standby the test
//     promotes (pg_promote) between the samples.

func disposablePool(t *testing.T, env string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s not set: no disposable server to restart or promote", env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", env, err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// sampleIdentityProbes runs the probes the two families compare.
func sampleIdentityProbes(ctx context.Context, r *probes.Runner, step int) []Observation {
	var out []Observation
	for i, id := range []probes.ID{probes.ConnectionSaturation, probes.WALCheckpoint,
		probes.ReplicationSlots, probes.Archiver} {
		out = append(out, Observation{EvidenceID: string(rune('A' + step*4 + i)),
			Result: r.Run(ctx, id, probes.Args{})})
	}
	return out
}

func requireUsable(t *testing.T, obs []Observation) {
	t.Helper()
	for _, o := range obs[:2] {
		if !o.Result.Status.Usable() {
			t.Fatalf("%s = %+v", o.Result.ProbeID, o.Result)
		}
	}
}

func TestContainer_RestartBetweenSamplesInvalidatesComparisons(t *testing.T) {
	pool, ctx := disposablePool(t, "SAGE_TEST_RESTARTABLE_SUPERUSER_URL")
	var before time.Time
	if err := pool.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&before); err != nil {
		t.Fatalf("start time: %v", err)
	}
	first := sampleIdentityProbes(ctx, probes.NewRunner(pool, probes.Catalog(), nil), 0)
	requireUsable(t, first)
	// A fast shutdown of the postmaster (PID 1 in the container); the
	// restart policy brings the same data directory back up.
	_, _ = pool.Exec(ctx, "COPY (SELECT 1) TO PROGRAM 'kill -INT 1'")
	waitForNewStart(t, ctx, pool, before)
	// A fresh runner, as after a sidecar reconnect.
	second := sampleIdentityProbes(ctx, probes.NewRunner(pool, probes.Catalog(), nil), 1)
	requireUsable(t, second)
	obs := append(first, second...)
	conn := DiagnoseConnections(obs)
	if got := missingReason(conn, probes.ConnectionSaturation); got != ReasonServerRestarted {
		t.Fatalf("connections across a restart: missing %q (%+v)", got, conn.Missing)
	}
	if leak, _ := byNode(conn, ConnectionLeak); leak.Confidence != 0 {
		t.Fatalf("leak scored across a real restart: %+v", leak)
	}
	wal := DiagnoseWAL(obs)
	if got := missingReason(wal, probes.WALCheckpoint); got != ReasonServerRestarted {
		t.Fatalf("WAL across a restart: missing %q (%+v)", got, wal.Missing)
	}
	if surge, _ := byNode(wal, WriteSurge); surge.Confidence != 0 {
		t.Fatalf("surge scored across a real restart: %+v", surge)
	}
}

func waitForNewStart(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	before time.Time) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var now time.Time
		err := pool.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&now)
		if err == nil && now.After(before) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("the disposable server did not come back with a new start time")
}

func TestContainer_PromotionBetweenSamplesInvalidatesComparisons(t *testing.T) {
	pool, ctx := disposablePool(t, "SAGE_TEST_CAUSAL_STANDBY_URL")
	var standby bool
	if err := pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil ||
		!standby {
		t.Skipf("SAGE_TEST_CAUSAL_STANDBY_URL is not a standby (already promoted?): %v",
			err)
	}
	runner := probes.NewRunner(pool, probes.Catalog(), nil)
	first := sampleIdentityProbes(ctx, runner, 0)
	requireUsable(t, first)
	var promoted bool
	if err := pool.QueryRow(ctx, "SELECT pg_promote(true, 60)").Scan(&promoted); err != nil ||
		!promoted {
		t.Fatalf("promote the disposable standby: %v %v", promoted, err)
	}
	second := sampleIdentityProbes(ctx, runner, 1)
	requireUsable(t, second)
	obs := append(first, second...)
	conn := DiagnoseConnections(obs)
	if got := missingReason(conn, probes.ConnectionSaturation); got != ReasonFailover {
		t.Fatalf("connections across a promotion: missing %q (%+v)", got, conn.Missing)
	}
	wal := DiagnoseWAL(obs)
	if got := missingReason(wal, probes.WALCheckpoint); got != ReasonFailover {
		t.Fatalf("WAL across a promotion: missing %q (%+v)", got, wal.Missing)
	}
	gs, _ := probes.ConnectionGroups(second[0].Result)
	before, _ := probes.ConnectionGroups(first[0].Result)
	if len(gs) == 0 || len(before) == 0 || gs[0].Identity.TimelineID <=
		before[0].Identity.TimelineID {
		t.Fatalf("the promoted timeline did not advance: %+v -> %+v", before, gs)
	}
}
