package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/leader"
)

// migration-07: rehearsal clones are receipted in the receipt database
// under its deployment id, and the leader's reaper destroys the clone of
// an expired receipt a crash left behind.

type reaperAdapter struct {
	mu        sync.Mutex
	n         int
	created   []string
	destroyed []string
}

func (a *reaperAdapter) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	id := fmt.Sprintf("dle-%d", a.n)
	a.created = append(a.created, id)
	return clone.Clone{ID: id, DSN: "postgres://clone.invalid/app",
		CreatedFrom: time.Now()}, nil
}

func (a *reaperAdapter) Destroy(_ context.Context, c clone.Clone) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.destroyed = append(a.destroyed, c.ID)
	return nil
}

func (a *reaperAdapter) SnapshotAge(context.Context) (time.Duration, error) {
	return time.Minute, nil
}

var reaperCloneConfig = config.CloneProviderConfig{Provider: "dle",
	DLEEndpoint: "https://ops:pw@dle.invalid:2345/", DLEToken: "test-token",
	MaxCloneAgeMinutes: 60}

func reaperPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := agentWiringPool(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.clone_instances")
	}
	clean()
	t.Cleanup(clean)
	return pool
}

// crashedRehearsal creates a receipted clone and never destroys it, then
// lets its receipt expire.
func crashedRehearsal(t *testing.T, pool *pgxpool.Pool, adapter clone.Provider) clone.Clone {
	t.Helper()
	ctx := context.Background()
	p, err := receiptingRehearsalProvider(adapter, pool, reaperCloneConfig)
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 minute' WHERE ref = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	return c
}

func receiptStatus(t *testing.T, pool *pgxpool.Pool, ref string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM
		sage.clone_instances WHERE ref = $1`, ref).Scan(&status); err != nil {
		t.Fatalf("receipt of %s: %v", ref, err)
	}
	return status
}

func TestRehearsalReceiptIsWrittenUnderTheDeployment(t *testing.T) {
	pool := reaperPool(t)
	ctx := context.Background()
	p, err := receiptingRehearsalProvider(&reaperAdapter{}, pool, reaperCloneConfig)
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var dep, adapter, scope, status, purpose string
	var ttl time.Duration
	if err := pool.QueryRow(ctx, `SELECT deployment_id::text, adapter, scope, status,
		purpose, expires_at - created_at FROM sage.clone_instances WHERE ref = $1`, c.ID).
		Scan(&dep, &adapter, &scope, &status, &purpose, &ttl); err != nil {
		t.Fatal(err)
	}
	if dep != deployment || adapter != "dle" || scope != "https://dle.invalid:2345" ||
		status != clone.StatusReady || purpose != clone.PurposeRehearsal ||
		ttl < 74*time.Minute || ttl > 76*time.Minute {
		t.Fatalf("receipt %s %s %s %s %s %s", dep, adapter, scope, status, purpose, ttl)
	}
	if err := p.Destroy(ctx, c); err != nil || receiptStatus(t, pool, c.ID) !=
		clone.StatusDestroyed {
		t.Fatalf("destroy: %v", err)
	}
}

func TestReapRehearsalClonesDestroysOnlyThisDeploymentsClone(t *testing.T) {
	pool := reaperPool(t)
	ctx := context.Background()
	adapter := &reaperAdapter{}
	crashed := crashedRehearsal(t, pool, adapter)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.clone_instances (deployment_id,
		adapter, scope, ref, name, purpose, status, expires_at) VALUES
		('00000000-0000-4000-8000-0000000000aa', 'dle', 'https://dle.invalid:2345',
		 'dle-foreign', 'rehearsal-foreign', 'rehearsal', 'ready',
		 now() - interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	report := reapRehearsalClones(ctx, reaperCloneConfig, adapter, pool)
	if len(adapter.destroyed) != 1 || adapter.destroyed[0] != crashed.ID ||
		len(report.Destroyed) != 1 {
		t.Fatalf("destroyed %v report %+v, want only %s", adapter.destroyed, report,
			crashed.ID)
	}
	if got := receiptStatus(t, pool, crashed.ID); got != clone.StatusDestroyed {
		t.Fatalf("crashed receipt %s", got)
	}
	if got := receiptStatus(t, pool, "dle-foreign"); got != clone.StatusReady {
		t.Fatalf("another deployment's receipt was touched: %s", got)
	}
}

func TestCloneReapPassRunsOnTheLeaderOnly(t *testing.T) {
	pool := reaperPool(t)
	adapter := &reaperAdapter{}
	crashed := crashedRehearsal(t, pool, adapter)
	old := fleetLeader
	t.Cleanup(func() { fleetLeader = old })
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "live", Pool: pool,
		Config: config.DatabaseConfig{Name: "live"}, Status: &fleet.InstanceStatus{}})
	c := &config.Config{Clone: reaperCloneConfig}

	fleetLeader = leader.NewElector(stubLease{held: false}, "s", "me", time.Minute)
	_ = fleetLeader.Tick(context.Background())
	if n := cloneReapPass(context.Background(), c, mgr, nil, adapter); n != 0 ||
		len(adapter.destroyed) != 0 {
		t.Fatalf("a follower reaped %d databases: %v", n, adapter.destroyed)
	}
	fleetLeader = leader.NewElector(stubLease{held: true}, "s", "me", time.Minute)
	_ = fleetLeader.Tick(context.Background())
	if n := cloneReapPass(context.Background(), c, mgr, nil, adapter); n != 1 ||
		len(adapter.destroyed) != 1 || adapter.destroyed[0] != crashed.ID {
		t.Fatalf("leader reaped %d databases: %v", n, adapter.destroyed)
	}
}

func TestReapUnresolvedReceiptIsReportedOnceNeverDestroyed(t *testing.T) {
	pool := reaperPool(t)
	ctx := context.Background()
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("rehearsal-uncertain-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO sage.clone_instances (deployment_id,
		adapter, scope, name, purpose, status, expires_at) VALUES ($1::uuid, 'dle',
		'https://dle.invalid:2345', $2, 'rehearsal', 'create_uncertain',
		now() - interval '1 minute')`, deployment, name); err != nil {
		t.Fatal(err)
	}
	adapter := &reaperAdapter{}
	var report clone.ReapReport
	first := captureStderr(t, func() {
		report = reapRehearsalClones(ctx, reaperCloneConfig, adapter, pool)
	})
	second := captureStderr(t, func() {
		reapRehearsalClones(ctx, reaperCloneConfig, adapter, pool)
	})
	if len(report.Unresolved) != 1 || len(adapter.destroyed) != 0 {
		t.Fatalf("report %+v destroyed %v", report, adapter.destroyed)
	}
	if !strings.Contains(first, name) || !strings.Contains(first, "not destroyed") ||
		strings.Contains(second, name) {
		t.Fatalf("first log %q, second log %q", first, second)
	}
}

// No DB: the receipt database choice and the receipt scope are pure.
func TestCloneReceiptPoolsAndScope(t *testing.T) {
	mgr, pools := managerWith(t, "b", "a")
	got := cloneReceiptPools(&config.Config{}, mgr, nil)
	if len(got) != 2 || got[0] != pools["a"] || got[1] != pools["b"] {
		t.Fatalf("without a control database: %v, want a then b", got)
	}
	control := &config.Config{}
	control.Agents.ControlDatabase = "b"
	if got := cloneReceiptPools(control, mgr, nil); len(got) != 1 || got[0] != pools["b"] {
		t.Fatalf("with a control database: %v", got)
	}
	if got := cloneReceiptPools(&config.Config{}, nil, nil); len(got) != 0 {
		t.Fatalf("no fleet: %v", got)
	}
	for endpoint, want := range map[string]string{
		"https://ops:pw@dle.invalid:2345/api/?q=1#f": "https://dle.invalid:2345/api",
		"https://dle.invalid":                        "https://dle.invalid",
		"":                                           "dle",
		"dle.invalid":                                "dle",
	} {
		if got := cloneReceiptScope(config.CloneProviderConfig{Provider: "dle",
			DLEEndpoint: endpoint}); got != want {
			t.Errorf("scope of %q = %q, want %q", endpoint, got, want)
		}
	}
	if _, err := receiptingRehearsalProvider(nil, nil, reaperCloneConfig); err == nil ||
		errors.Is(err, clone.ErrNoReceiptStore) {
		t.Fatalf("nil adapter: %v", err)
	}
}
