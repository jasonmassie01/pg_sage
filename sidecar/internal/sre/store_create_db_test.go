package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Durable investigation store (Codex §5-§7) against real PostgreSQL:
// deployment and database identity, trigger coalescing and idempotency.

func liveStore(t *testing.T, limits Limits) (*PostgresStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	st, err := NewPostgresStore(pool, limits)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	return st, pool, ctx
}

// testScope binds a fresh database identity under this deployment.
func testScope(t *testing.T, ctx context.Context, st *PostgresStore) Scope {
	t.Helper()
	dep, err := st.EnsureDeployment(ctx)
	if err != nil {
		t.Fatalf("EnsureDeployment: %v", err)
	}
	scope, err := st.BindDatabase(ctx, Binding{DeploymentID: dep,
		RuntimeKey: fmt.Sprintf("db-%s", NewUUID()), Strength: StrengthConfigured,
		ClusterEpoch: "epoch-1"})
	if err != nil {
		t.Fatalf("BindDatabase: %v", err)
	}
	return scope
}

func lockStart(scope Scope, subject string) StartRequest {
	return StartRequest{Scope: scope, CaseID: "case:lock:" + subject,
		TriggerKind: TriggerLock, Subject: subject}
}

func TestStore_DeploymentAndBindingAreStable(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	a, err := st.EnsureDeployment(ctx)
	if err != nil {
		t.Fatalf("EnsureDeployment: %v", err)
	}
	restarted, _ := NewPostgresStore(pool, DefaultLimits())
	b, err := restarted.EnsureDeployment(ctx)
	if err != nil || a != b {
		t.Fatalf("deployment id changed across restart: %s -> %s (%v)", a, b, err)
	}
	bind := Binding{DeploymentID: a, RuntimeKey: "orders-" + string(NewUUID()),
		Strength: StrengthConfigured, ClusterEpoch: "e1"}
	s1, err := st.BindDatabase(ctx, bind)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	s2, err := restarted.BindDatabase(ctx, bind)
	if err != nil || s1 != s2 {
		t.Fatalf("rebinding the same runtime key gave %v then %v (%v)", s1, s2, err)
	}
	bind.RuntimeKey += "-other"
	s3, err := st.BindDatabase(ctx, bind)
	if err != nil || s3.DatabaseID == s1.DatabaseID {
		t.Fatalf("another runtime key reused database id %s (%v)", s3.DatabaseID, err)
	}
}

func TestStore_BindDatabaseRejectsInvalidInput(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	dep, _ := st.EnsureDeployment(ctx)
	cases := map[string]Binding{
		"no deployment": {RuntimeKey: "k", Strength: StrengthConfigured, ClusterEpoch: "e"},
		"empty key":     {DeploymentID: dep, Strength: StrengthConfigured, ClusterEpoch: "e"},
		"long key": {DeploymentID: dep, RuntimeKey: strings.Repeat("k", 129),
			Strength: StrengthConfigured, ClusterEpoch: "e"},
		"bad strength": {DeploymentID: dep, RuntimeKey: "k", Strength: "guess",
			ClusterEpoch: "e"},
		"no epoch": {DeploymentID: dep, RuntimeKey: "k", Strength: StrengthConfigured},
	}
	for name, b := range cases {
		if _, err := st.BindDatabase(ctx, b); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestNewPostgresStore_RejectsNilPoolAndBadLimits(t *testing.T) {
	if _, err := NewPostgresStore(nil, DefaultLimits()); err == nil {
		t.Fatal("nil pool accepted")
	}
	bad := DefaultLimits()
	bad.MaxProbes = 99
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	defer pool.Close()
	if _, err := NewPostgresStore(pool, bad); err == nil {
		t.Fatal("limits above the R1 ceilings accepted")
	}
}

// CHECK-13: duplicate triggers coalesce without losing scoped identity.
func TestStore_CreateCoalescesDuplicateTriggers(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	first, created, err := st.Create(ctx, lockStart(scope, "pid 10"))
	if err != nil || !created || first.State != StateQueued || first.Version != 1 {
		t.Fatalf("first create = %+v created=%v err=%v", first, created, err)
	}
	dup := lockStart(scope, "pid 10")
	dup.CaseID = "case:other"
	again, created, err := st.Create(ctx, dup)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("duplicate trigger = %+v created=%v err=%v, want %s",
			again, created, err, first.ID)
	}
	other, created, err := st.Create(ctx, lockStart(scope, "pid 11"))
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("a different subject coalesced: %+v %v", other, err)
	}
	scopeB := testScope(t, ctx, st)
	inB, created, err := st.Create(ctx, lockStart(scopeB, "pid 10"))
	if err != nil || !created || inB.ID == first.ID || inB.Scope != scopeB {
		t.Fatalf("same trigger in another database coalesced: %+v %v", inB, err)
	}
	if _, err := st.Get(ctx, scopeB, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get with the wrong scope = %v, want ErrNotFound", err)
	}
}

func TestStore_CreateConcurrentDuplicatesYieldOne(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	const n = 8
	ids := make([]UUID, n)
	createdCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv, created, err := st.Create(ctx, lockStart(scope, "pid 77"))
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			mu.Lock()
			ids[i] = inv.ID
			if created {
				createdCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("racing duplicate triggers produced %v", ids)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created = %d, want exactly 1", createdCount)
	}
}

func TestStore_IdempotencyKeySurvivesConclusion(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	req := lockStart(scope, "pid 12")
	req.IdempotencyKey = "operator-request-042"
	inv, _, err := st.Create(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	driveToConcluded(t, ctx, st, inv)
	again, created, err := st.Create(ctx, req)
	if err != nil || created || again.ID != inv.ID || again.State != StateConcluded {
		t.Fatalf("repeated key after conclusion = %+v created=%v err=%v", again,
			created, err)
	}
	req.IdempotencyKey = "operator-request-043"
	fresh, created, err := st.Create(ctx, req)
	if err != nil || !created || fresh.ID == inv.ID {
		t.Fatalf("a new request after conclusion = %+v created=%v err=%v", fresh,
			created, err)
	}
}

func driveToConcluded(t *testing.T, ctx context.Context, st *PostgresStore,
	inv Investigation) {
	t.Helper()
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, lease, StepResult{IdempotencyKey: "s1",
		NextState: StateEvaluating}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := st.Release(ctx, lease, StateConcluded); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestStore_ExpiredQueuedTriggerIsReplaced(t *testing.T) {
	limits := DefaultLimits()
	limits.QueueExpiry = 200 * time.Millisecond
	st, _, ctx := liveStore(t, limits)
	scope := testScope(t, ctx, st)
	old, _, err := st.Create(ctx, lockStart(scope, "pid 13"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	fresh, created, err := st.Create(ctx, lockStart(scope, "pid 13"))
	if err != nil || !created || fresh.ID == old.ID {
		t.Fatalf("expired trigger was reused: %+v created=%v err=%v", fresh, created, err)
	}
	expired, err := st.Get(ctx, scope, old.ID)
	if err != nil || expired.State != StateExpired {
		t.Fatalf("old investigation = %+v (%v), want expired", expired, err)
	}
	if _, err := st.Claim(ctx, scope, old.ID, NewUUID()); !errors.Is(err, ErrTerminal) {
		t.Fatalf("claiming an expired investigation = %v, want ErrTerminal", err)
	}
}

func TestStore_CreateRejectsInvalidRequests(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	bad := lockStart(scope, "pid 1")
	bad.TriggerKind = "shell"
	if _, _, err := st.Create(ctx, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown trigger kind: %v", err)
	}
	unbound := lockStart(Scope{DeploymentID: scope.DeploymentID,
		DatabaseID: NewUUID()}, "pid 1")
	if _, _, err := st.Create(ctx, unbound); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unbound database scope: %v, want ErrInvalidRequest", err)
	}
}
