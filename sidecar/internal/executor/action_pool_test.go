package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// lazyPool is a pool that never connected: nothing listens on port 1, so
// its first use fails with a connection error.
func lazyPool(t *testing.T, user string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://" + user +
		"@127.0.0.1:1/app?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("parse lazy DSN: %v", err)
	}
	cfg.MinConns = 0
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Nil/empty: without an action pool every action runs on the one pool,
// today's single-identity behaviour (regression guard).
func TestActionDB_SingleIdentityUsesThePool(t *testing.T) {
	obs := lazyPool(t, "observer")
	e := New(obs, config.DefaultConfig(), time.Time{}, nopLog)
	if e.HasActionIdentity() {
		t.Fatal("HasActionIdentity = true without an action pool")
	}
	if got := e.actionDB(); got != obs {
		t.Fatalf("actionDB = %p, want the observation pool %p", got, obs)
	}
	e.WithActionPool(nil)
	if e.HasActionIdentity() || e.actionDB() != obs {
		t.Fatal("WithActionPool(nil) changed the single identity")
	}
}

// Happy path: with an action pool, actions take it and observation keeps
// its own pool.
func TestActionDB_PrefersTheActionPool(t *testing.T) {
	obs, act := lazyPool(t, "observer"), lazyPool(t, "actor")
	e := New(obs, config.DefaultConfig(), time.Time{}, nopLog).WithActionPool(act)
	if !e.HasActionIdentity() {
		t.Fatal("HasActionIdentity = false with an action pool")
	}
	if got := e.actionDB(); got != act {
		t.Fatalf("actionDB = %p, want the action pool %p", got, act)
	}
	if e.pool != obs {
		t.Fatal("the observation pool was replaced by the action pool")
	}
	cfg := e.rollbackMonitorConfig(func(context.Context, string) bool { return true })
	if cfg.ActionPool != act {
		t.Fatalf("rollback config action pool = %p, want %p", cfg.ActionPool, act)
	}
}

func executeIntent(executed *int) ActionIntent {
	return ActionIntent{
		Request: policy.ActionRequest{SQL: "CREATE INDEX CONCURRENTLY ix ON public.t (a)"},
		Authorize: func(context.Context) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Decision: PolicyDecisionExecute, RiskTier: "safe",
				DecisionID: 7}, nil
		},
		SlotHeld: true,
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			*executed++
			return 42, nil
		},
	}
}

// Error propagation: an action pool that cannot connect fails Apply with
// "action identity unavailable" before the change runs.
func TestApply_ActionIdentityUnavailable(t *testing.T) {
	obs, act := lazyPool(t, "observer"), lazyPool(t, "actor")
	e := New(obs, config.DefaultConfig(), time.Time{}, nopLog).WithActionPool(act)
	executed := 0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := e.Apply(ctx, executeIntent(&executed))
	if !errors.Is(err, ErrActionIdentityUnavailable) {
		t.Fatalf("Apply error = %v, want ErrActionIdentityUnavailable", err)
	}
	if !strings.Contains(err.Error(), "action identity unavailable") {
		t.Fatalf("error %q does not say the action identity is unavailable", err)
	}
	if executed != 0 || id != 0 {
		t.Fatalf("Execute ran %d times (id %d) without the action identity", executed, id)
	}
	if n := e.InFlightActions(); n != 0 {
		t.Fatalf("%d runs left in flight after the refusal", n)
	}
}

// Nil/empty: without an action pool Apply never probes one and runs.
func TestApply_SingleIdentityRunsWithoutProbe(t *testing.T) {
	e := New(lazyPool(t, "observer"), config.DefaultConfig(), time.Time{}, nopLog)
	executed := 0
	id, err := e.Apply(context.Background(), executeIntent(&executed))
	if err != nil || executed != 1 || id != 42 {
		t.Fatalf("Apply = (%d, %v), executed %d; want (42, nil), 1", id, err, executed)
	}
}

// Boundary: a cancelled request is the caller's cancellation, not an
// unavailable identity.
func TestCheckActionIdentity_CancelledContext(t *testing.T) {
	e := New(lazyPool(t, "observer"), config.DefaultConfig(), time.Time{}, nopLog).
		WithActionPool(lazyPool(t, "actor"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.checkActionIdentity(ctx)
	if err == nil {
		t.Fatal("checkActionIdentity(cancelled) = nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("checkActionIdentity(cancelled) = %v, want context.Canceled", err)
	}
}

// Boundary: the action pool fits every DDL slot's two connections and one
// free connection for backend signals.
func TestActionPoolMaxConnsCoversEveryDDLSlot(t *testing.T) {
	if got, want := ActionPoolMaxConns(), int32(2*maxConcurrentDDL+1); got != want {
		t.Fatalf("ActionPoolMaxConns = %d, want %d", got, want)
	}
	if ActionPoolMaxConns() <= int32(2*maxConcurrentDDL) {
		t.Fatal("no connection is left for backend signals")
	}
}
