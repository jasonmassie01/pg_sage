package inbox

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// fleetpol-01c: policy-set exceptions on a real server reach the inbox as
// policy_exception items, next to policy proposals, for the database they
// belong to only, and leave once they expire.
// No concurrent access tests here: the service's fan-out is covered by
// TestInboxDB_ThousandItemsOverFiveHundredDatabases.

func registerDatabase(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	return scalarInt(t, pool, `INSERT INTO sage.databases (name, host, database_name,
		username, password_enc, tags) VALUES ($1, 'localhost', $1, 'sage', '\x00',
		'{"tier": "prod"}') RETURNING id`, name)
}

func exceptionFleet(t *testing.T, pool *pgxpool.Pool, ids map[string]int64) *Service {
	t.Helper()
	mgr := fleet.NewManager(config.DefaultConfig())
	for name, id := range ids {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, DatabaseID: int(id),
			Pool: pool, Status: &fleet.InstanceStatus{}})
	}
	return &Service{Sources: FleetSources(mgr, nil)}
}

func setException(t *testing.T, pool *pgxpool.Pool, id int64, expires *time.Time) {
	t.Helper()
	store := policy.NewStore(pool)
	ctx := context.Background()
	if _, err := store.Bootstrap(ctx, policy.Scope{DatabaseID: &id}, "staffed",
		"bootstrap"); err != nil {
		t.Fatalf("bootstrap sets: %v", err)
	}
	if _, err := store.SetException(ctx, policy.ExceptionRequest{DatabaseID: id,
		Actor: "alice@example.com", Reason: "month-end batch", ExpiresAt: expires}); err != nil {
		t.Fatalf("set exception: %v", err)
	}
}

func TestInboxDB_PolicyExceptionsAreItemsOfTheirDatabase(t *testing.T) {
	pool := inboxDB(t, "inbox_exceptions")
	billing := registerDatabase(t, pool, "billing")
	orders := registerDatabase(t, pool, "orders")
	setException(t, pool, billing, nil)
	mustExec(t, pool, `INSERT INTO sage.policy (database_id, version, doc, status,
		proposed_by, widening) VALUES (NULL, -1, '{}', 'proposed', 'dba', false)`)
	svc := exceptionFleet(t, pool, map[string]int64{"billing": billing, "orders": orders})

	res := svc.List(context.Background(), Request{Databases: []string{"billing"},
		Kinds: []Kind{KindPolicyException}})
	if res.LowerBound || len(res.Errors) != 0 || res.Total != 2 {
		t.Fatalf("billing: %+v, want the proposal and the exception", res)
	}
	var exc *Item
	for i := range res.Items {
		if res.Items[i].ID == "exception-"+strconv.FormatInt(billing, 10) {
			exc = &res.Items[i]
		}
	}
	if exc == nil || exc.Risk != RiskModerate || exc.Database != "billing" ||
		exc.Source != "sage.policy_exceptions" || exc.DueAt != nil {
		t.Fatalf("exception item %+v in %+v", exc, res.Items)
	}

	res = svc.List(context.Background(), Request{Databases: []string{"orders"},
		Kinds: []Kind{KindPolicyException}})
	for _, it := range res.Items {
		if it.Source == "sage.policy_exceptions" {
			t.Fatalf("orders has no exception, got %+v", it)
		}
	}
}

func TestInboxDB_ExpiredExceptionLeavesTheInbox(t *testing.T) {
	pool := inboxDB(t, "inbox_exc_expired")
	billing := registerDatabase(t, pool, "billing")
	later := time.Now().Add(time.Hour)
	setException(t, pool, billing, &later)
	svc := exceptionFleet(t, pool, map[string]int64{"billing": billing})
	req := Request{Databases: []string{"billing"}, Kinds: []Kind{KindPolicyException}}

	res := svc.List(context.Background(), req)
	if res.Total != 1 || res.Items[0].Risk != RiskSafe || res.Items[0].DueAt == nil {
		t.Fatalf("bounded exception: %+v, want one safe item with a due time", res)
	}
	// The policy store judges expiry on the sidecar's clock, so the stamp
	// comes from it too: the test server's clock can run seconds ahead.
	mustExec(t, pool, `UPDATE sage.policy_exceptions
		SET expires_at = $2 WHERE database_id = $1`, billing, time.Now().Add(-time.Second))
	if res = svc.List(context.Background(), req); res.Total != 0 || res.LowerBound {
		t.Fatalf("expired exception still listed: %+v", res)
	}
}

func TestInboxDB_UnregisteredInstanceHasNoExceptions(t *testing.T) {
	pool := inboxDB(t, "inbox_exc_unregistered")
	billing := registerDatabase(t, pool, "billing")
	setException(t, pool, billing, nil)
	// The standalone instance carries no registry id: nothing is its exception.
	svc := exceptionFleet(t, pool, map[string]int64{"standalone": 0})
	res := svc.List(context.Background(), Request{Databases: []string{"standalone"},
		Kinds: []Kind{KindPolicyException}})
	if res.Total != 0 || res.LowerBound || len(res.Errors) != 0 {
		t.Fatalf("standalone: %+v, want nothing and no error", res)
	}
}
