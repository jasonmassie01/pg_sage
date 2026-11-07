package rca

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-01 state transition: the locks category is now a wait graph
// (waiter rows, then one session row per blocker, idle-in-transaction
// session and oldest xmin holder). treeVacuumBlocked still names the
// idle-in-transaction session from those rows.

// No concurrent access test: treeVacuumBlocked reads one snapshot.

func vacuumSignal(now time.Time) *Signal {
	return &Signal{ID: "vacuum_blocked", FiredAt: now, Severity: "warning",
		Metrics: map[string]any{"blocked_tables": []string{"public.acct"}}}
}

func TestTreeVacuumBlocked_NamesIdleHolderFromWaitGraphRows(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Hour)
	age := int64(1200)
	for name, locks := range map[string][]collector.LockInfo{
		"waiter first": {
			{LockType: "transactionid", Mode: "ShareLock", PID: 22,
				State: strPtr("active"), BlockedBy: []int{11}},
			{LockType: "virtualxid", Mode: "ExclusiveLock", Granted: true, PID: 11,
				State: strPtr("idle in transaction"), BackendStart: &start,
				XactStart: &start, BackendXminAge: &age},
		},
		"holder first": {
			{LockType: "virtualxid", Mode: "ExclusiveLock", Granted: true, PID: 11,
				State: strPtr("idle in transaction"), BackendStart: &start,
				XactStart: &start, BackendXminAge: &age},
			{LockType: "transactionid", Mode: "ShareLock", PID: 22,
				State: strPtr("active"), BlockedBy: []int{11}},
		},
	} {
		inc := newTestEngine().treeVacuumBlocked(vacuumSignal(now),
			&collector.Snapshot{CollectedAt: now, Locks: locks}, testVacuumRules())
		if !strings.Contains(inc.RootCause, "PID 11") {
			t.Errorf("%s: root cause %q, want the idle-in-transaction PID 11",
				name, inc.RootCause)
		}
		if len(inc.CausalChain) != 1 || inc.CausalChain[0].Blocker == nil ||
			inc.CausalChain[0].Blocker.PID != 11 ||
			!inc.CausalChain[0].Blocker.BackendStart.Equal(start) {
			t.Errorf("%s: chain %+v, want the holder's identity (pid 11, its start)",
				name, inc.CausalChain)
		}
	}
}

// Integration: rows the collector reads from a live server still give the
// idle-in-transaction holder.
func TestTreeVacuumBlocked_NamesIdleHolderFromLiveCollector(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "rca_wait_graph")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	holder := liveIdleHolder(t, ctx, pool, dsn)
	snap := firstSnapshot(t, pool)
	// rca-01: a holder is named only past rca.xmin_holder_min_minutes;
	// this session is seconds old, so the test states a zero floor. It
	// used the tree's old rule, which named any idle session at once.
	rules := testVacuumRules()
	rules.holderFloor = 0
	inc := newTestEngine().treeVacuumBlocked(vacuumSignal(snap.CollectedAt), snap, rules)
	if !strings.Contains(inc.RootCause, fmt.Sprintf("PID %d", holder)) {
		t.Fatalf("root cause %q, want the idle-in-transaction PID %d (locks %+v)",
			inc.RootCause, holder, snap.Locks)
	}
}

func liveIdleHolder(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	dsn string) int {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE TABLE public.acct (id int PRIMARY KEY, v int);
		INSERT INTO public.acct VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"BEGIN", "UPDATE public.acct SET v = 1 WHERE id = 1"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return pid
}

// firstSnapshot runs the collector until its first snapshot.
func firstSnapshot(t *testing.T, pool *pgxpool.Pool) *collector.Snapshot {
	t.Helper()
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Collector: config.CollectorConfig{IntervalSeconds: 60, BatchSize: 100, MaxQueries: 50},
		Safety: config.SafetyConfig{CPUCeilingPct: 90, BackoffConsecutiveSkips: 5,
			DormantIntervalSeconds: 600},
	}
	c := collector.New(pool, cfg, version, func(string, string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if s := c.LatestSnapshot(); s != nil {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the collector took no snapshot within 60 s")
	return nil
}
