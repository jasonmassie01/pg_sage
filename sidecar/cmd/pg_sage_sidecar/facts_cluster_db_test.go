package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// facts-02 on a real cluster (wal_level=logical): database A has a CDC
// slot and its confirmed slot_consumer fact; database B's WAL custodian,
// which reads every slot of the cluster but only B's registry, proposes a
// cluster-wide bound. B's gate blocks it with A's fact.

type clusterPair struct {
	ctx            context.Context
	poolA, poolB   *pgxpool.Pool
	storeA, storeB *facts.Store
	cdc, physical  string
}

func clusterPool(t *testing.T, ctx context.Context, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return p
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// newClusterPair creates A and B, a physical slot that retains the most
// WAL and a logical CDC slot in A, and writes WAL behind both.
func newClusterPair(t *testing.T) *clusterPair {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	c := &clusterPair{ctx: ctx, poolA: clusterPool(t, ctx, "factsa"),
		poolB: clusterPool(t, ctx, "factsb")}
	suffix := randomSuffix(t)
	c.cdc, c.physical = "cdc_orders_"+suffix, "old_replica_"+suffix
	c.storeA, c.storeB = facts.NewStore(c.poolA), facts.NewStore(c.poolB)
	t.Cleanup(func() { dropTestSlots(t, c.poolA, c.cdc, c.physical) })
	if _, err := c.poolA.Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true)",
		c.physical); err != nil {
		t.Fatalf("create physical slot: %v", err)
	}
	if _, err := c.poolA.Exec(ctx, "SELECT pg_create_logical_replication_slot($1, "+
		"'pgoutput')", c.cdc); err != nil {
		t.Fatalf("create logical slot: %v", err)
	}
	if _, err := c.poolA.Exec(ctx, "CREATE TABLE public.wal_filler AS "+
		"SELECT g, md5(g::text) AS v FROM generate_series(1, 5000) g"); err != nil {
		t.Fatalf("write WAL: %v", err)
	}
	return c
}

func dropTestSlots(t *testing.T, p *pgxpool.Pool, names ...string) {
	if _, err := p.Exec(context.Background(), `SELECT pg_drop_replication_slot(slot_name)
		FROM pg_replication_slots WHERE slot_name = ANY($1)`, names); err != nil {
		t.Errorf("drop test slots %v: %v", names, err)
	}
}

func confirmSlotFact(t *testing.T, c *clusterPair) facts.Fact {
	t.Helper()
	f, _, err := c.storeA.Propose(c.ctx, facts.Proposal{Type: facts.TypeSlotConsumer,
		Kind: facts.KindSlot, Subject: c.cdc, Source: facts.SourceOperator,
		ProposedBy: "alice@example.com", Value: map[string]string{"consumer": "debezium"}})
	if err != nil {
		t.Fatalf("propose slot fact: %v", err)
	}
	f, err = c.storeA.Decide(c.ctx, f.ID, facts.Decision{Confirm: true,
		Actor: "alice@example.com"})
	if err != nil || f.Status != facts.StatusConfirmed {
		t.Fatalf("confirm slot fact: %+v %v", f, err)
	}
	return f
}

// walBoundOfB is the bound B's own WAL custodian proposes.
func walBoundOfB(t *testing.T, c *clusterPair) autonomy.Proposal {
	t.Helper()
	cust := autonomy.NewPostgresWALCustodian(c.poolB, "facts_b", autonomy.PostgresWALOptions{
		RetainedBytesLimit: 1, AbandonAfter: time.Hour})
	proposals, err := cust.Scan(c.ctx)
	if err != nil {
		t.Fatalf("B's WAL custodian scan: %v", err)
	}
	for _, p := range proposals {
		if strings.Contains(p.SQL, "max_slot_wal_keep_size") {
			if len(p.TargetObjects) != 1 || !strings.HasPrefix(p.TargetObjects[0], "slot:") {
				t.Fatalf("bound targets %v", p.TargetObjects)
			}
			return p
		}
	}
	t.Fatalf("B's custodian proposed no WAL bound: %+v", proposals)
	return autonomy.Proposal{}
}

func gateOf(pool *pgxpool.Pool, binder policy.FactBinder) *executor.Executor {
	ex := autonomousExecutor(pool)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)
	ex.WithFactBinder(binder)
	return ex
}

func TestFactClusterSiblingSlotFactBlocksWALBoundOnRealCluster(t *testing.T) {
	c := newClusterPair(t)
	fact := confirmSlotFact(t, c)
	keyA, keyB := factClusterIdentity(c.ctx, c.poolA), factClusterIdentity(c.ctx, c.poolB)
	if keyA != keyB || keyA.sysid == "" {
		t.Fatalf("cluster identities %+v and %+v, want one system identifier", keyA, keyB)
	}
	r := newFactClusterRegistry()
	r.join(c.ctx, keyA, "facts_a", c.storeA)
	memberB := r.join(c.ctx, keyB, "facts_b", c.storeB)
	bound := walBoundOfB(t, c)

	// Before facts-02 B's gate saw only B's facts: none.
	plain, err := facts.NewBinder(c.storeB, nil, nil).Bind(c.ctx, policy.ActionRequest{
		SQL: bound.SQL, TargetObjs: bound.TargetObjects})
	if err != nil || len(plain) != 0 {
		t.Fatalf("B's own facts: %+v %v", plain, err)
	}
	binderB := newClusterFactBinder(c.poolB, c.storeB, r, memberB)
	got, err := binderB.Bind(c.ctx, policy.ActionRequest{SQL: bound.SQL,
		TargetObjs: bound.TargetObjects})
	if err != nil || len(got) != 1 || got[0].FactID != fact.ID ||
		got[0].Database != "facts_a" || got[0].Route != "alert" {
		t.Fatalf("B's bindings %+v %v, want A's fact %d", got, err, fact.ID)
	}
	d := gateOf(c.poolB, binderB).EvaluateCustodianProposal(c.ctx,
		executor.CustodianProposal{Feature: "wal", SQL: bound.SQL,
			TargetObjects: bound.TargetObjects, ObservedAt: time.Now()})
	want := fmt.Sprintf("fact #%d of database %q", fact.ID, "facts_a")
	if d.Decision != executor.PolicyDecisionBlocked ||
		d.BlockedReason != string(policy.ReasonBoundByFact) || !strings.Contains(d.Detail,
		want) {
		t.Fatalf("B's gate on %q (%v) = %+v, want bound_by_fact naming %s", bound.SQL,
			bound.TargetObjects, d, want)
	}
	t.Logf("CHECK-02: B's bound %q on %v blocked: %s", bound.SQL, bound.TargetObjects,
		d.Detail)
}

// CHECK-03: unbounding (-1) is never blocked by a slot fact, on the
// sibling or on the fact's own database.
func TestFactClusterUnboundIsNotBlockedOnRealCluster(t *testing.T) {
	c := newClusterPair(t)
	confirmSlotFact(t, c)
	r := newFactClusterRegistry()
	memberA := r.join(c.ctx, factClusterIdentity(c.ctx, c.poolA), "facts_a", c.storeA)
	memberB := r.join(c.ctx, factClusterIdentity(c.ctx, c.poolB), "facts_b", c.storeB)
	unbound := executor.CustodianProposal{Feature: "wal",
		SQL:           "ALTER SYSTEM SET max_slot_wal_keep_size = -1",
		TargetObjects: []string{"slot:" + c.physical}, ObservedAt: time.Now()}
	for name, side := range map[string]struct {
		pool   *pgxpool.Pool
		binder *facts.Binder
	}{
		"B": {c.poolB, newClusterFactBinder(c.poolB, c.storeB, r, memberB)},
		"A": {c.poolA, newClusterFactBinder(c.poolA, c.storeA, r, memberA)},
	} {
		binder := side.binder
		got, err := binder.Bind(c.ctx, policy.ActionRequest{SQL: unbound.SQL,
			TargetObjs: unbound.TargetObjects})
		if err != nil || len(got) != 0 {
			t.Fatalf("%s binds the unbound: %+v %v", name, got, err)
		}
		d := gateOf(side.pool, binder).EvaluateCustodianProposal(c.ctx, unbound)
		if d.BlockedReason == string(policy.ReasonBoundByFact) ||
			d.BlockedReason == string(policy.ReasonFactsUnavailable) {
			t.Fatalf("%s's gate on the unbound = %+v", name, d)
		}
	}
	// A bound A proposes for the physical slot is bound by A's own fact.
	got, err := newClusterFactBinder(c.poolA, c.storeA, r, memberA).Bind(c.ctx,
		policy.ActionRequest{SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'",
			TargetObjs: []string{"slot:" + c.physical}})
	if err != nil || len(got) != 1 || got[0].Database != "" {
		t.Fatalf("A's own bound: %+v %v", got, err)
	}
}
