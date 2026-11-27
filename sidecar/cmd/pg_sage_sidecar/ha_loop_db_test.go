package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/earned/hasource"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleet-04 on real PostgreSQL: the one persisted monitor per database.

// roleProbeTracer counts the role probes a pool runs.
type roleProbeTracer struct{ probes atomic.Int64 }

func (r *roleProbeTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn,
	data pgx.TraceQueryStartData) context.Context {
	if strings.TrimSpace(data.SQL) == "SELECT pg_is_in_recovery()" {
		r.probes.Add(1)
	}
	return ctx
}

func (r *roleProbeTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func tracedPool(t *testing.T) (*pgxpool.Pool, *roleProbeTracer) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	tracer := &roleProbeTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, tracer
}

func uniqueHADatabase(t *testing.T, control *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("ha_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = control.Exec(context.Background(),
			`DELETE FROM sage.ha_identity WHERE monitor_key = $1`, haIdentityKey(name, nil))
	})
	return name
}

// Footprint: a pgx tracer counts one pg_is_in_recovery() per cadence per
// database however many consumers read it (up to four probes before).
func TestHAOneRoleProbePerCadenceOnLivePostgres(t *testing.T) {
	control := autonomyPool(t)
	monitored, tracer := tracedPool(t)
	m := newInstanceHAMonitor(monitored, control, uniqueHADatabase(t, control), nil,
		time.Minute, 30*time.Minute)
	ctx := context.Background()
	for tick := 0; tick < 3; tick++ {
		if checkHA(ctx, m) {
			t.Fatal("the live primary reported not writable")
		}
		for read := 0; read < 10; read++ {
			_ = haReplicaGate(m)(ctx)
			_ = newRunwayAdvisor(nil, nil, haReplicaGate(m)).isReplica(ctx)
			if _, err := hasource.FromSnapshot(m).HAStatus(ctx); err != nil {
				t.Fatal(err)
			}
			_ = (&fleet.DatabaseInstance{HA: m}).SnapshotStatus()
		}
	}
	if got := tracer.probes.Load(); got != 3 {
		t.Fatalf("3 cadences with 120 consumer reads ran %d role probes, want 3", got)
	}
}

// Integration: a persisted identity with a changed timeline at startup
// makes the executor cycle receive isReplica=true (the failover cooldown).
// The orchestrator gate reads the shared monitor, not earned autonomy, so
// this holds with sre.autonomy.enforce=false; before fleet-04 only the
// earned limiter saw the cooldown.
func TestHAStartupTimelineChangeHoldsTheExecutorCycle(t *testing.T) {
	pool := autonomyPool(t)
	name := uniqueHADatabase(t, pool)
	ctx := context.Background()
	first := newInstanceHAMonitor(pool, pool, name, nil, time.Minute, 30*time.Minute)
	exec := &recordingExecutor{}
	runFleetDBCycle(ctx, fleetCycleDeps{name: name, ha: first, exec: exec})
	if exec.last(t) {
		t.Fatal("a first start on the primary held the executor cycle")
	}
	tag, err := pool.Exec(ctx, `UPDATE sage.ha_identity SET timeline_id = timeline_id + 1
		WHERE monitor_key = $1`, haIdentityKey(name, nil))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("simulate a failover while down: rows=%d err=%v", tag.RowsAffected(), err)
	}
	restarted := newInstanceHAMonitor(pool, pool, name, nil, time.Minute, 30*time.Minute)
	exec = &recordingExecutor{}
	runFleetDBCycle(ctx, fleetCycleDeps{name: name, ha: restarted, exec: exec})
	if !exec.last(t) {
		t.Fatal("a timeline change while pg_sage was down did not hold the executor cycle")
	}
	if !haReplicaGate(restarted)(ctx) {
		t.Fatal("the proposal router did not see the startup cooldown")
	}
	if s := restarted.Snapshot(); s.LastChange.IsZero() || s.Role != "primary" {
		t.Fatalf("snapshot = %+v, want primary with the cooldown opened", s)
	}
}

// checkedHAMonitor is a test database's HA monitor after one check, as
// the runtime hands it to its consumers (unpersisted, so tests sharing a
// server do not share a history).
func checkedHAMonitor(t *testing.T, pool *pgxpool.Pool) *ha.Monitor {
	t.Helper()
	m := newInstanceHAMonitor(pool, nil, "test", nil, time.Minute, 30*time.Minute)
	if checkHA(context.Background(), m) {
		t.Fatal("the test database is not a confirmed primary")
	}
	return m
}
