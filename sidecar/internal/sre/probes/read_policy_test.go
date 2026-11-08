package probes

import (
	"context"
	"testing"
)

func databasePolicy(database string) *ReadPolicy {
	return &ReadPolicy{Version: 1, Scope: "database", Database: database, Binding: "test-binding"}
}

func TestReadPolicyInventoryAndAuxiliarySources(t *testing.T) {
	if len(Catalog().IDs()) != 32 {
		t.Fatal("update native policy inventory")
	}
	for _, id := range append(append(Catalog().IDs(), ActionRegistry().IDs()...), ChangeFeed, SLOStatus, PoolerPools, "explain_statement", "custodian_freeze", "custodian_wal") {
		if SourceScope(id) == "unknown" {
			t.Errorf("unclassified source %s", id)
		}
	}
	p := databasePolicy("test")
	for _, id := range []ID{ConnectionSaturation, LockChains, SignalTarget, WALDirectoryProbe, SLOStatus, AutovacuumCancellations, "invented", "custodian_wal"} {
		if p.Allows(id) {
			t.Errorf("database policy permits %s", id)
		}
	}
	if !p.Allows(LockGraph) || !p.Allows(StatDatabase) {
		t.Fatal("database-safe sources unavailable")
	}
}

func TestReadPolicyCanonicalImmutableIntersection(t *testing.T) {
	p := databasePolicy("test")
	p.Allowed = []ID{StatTables, StatDatabase}
	q := p.Clone()
	q.Allowed[0], q.Allowed[1] = q.Allowed[1], q.Allowed[0]
	if p.Hash() != q.Hash() {
		t.Fatal("set order changed policy hash")
	}
	q.Allowed = []ID{StatDatabase}
	if p.Hash() == q.Hash() {
		t.Fatal("narrowing not reflected in durable policy")
	}
	ctx := WithReadPolicy(context.Background(), q)
	if CheckReadPolicy(ctx, p, StatTables, Args{}) == nil {
		t.Fatal("context narrowing bypassed")
	}
	q.Allowed[0] = StatTables
	if CheckReadPolicy(ctx, nil, StatTables, Args{}) == nil {
		t.Fatal("context policy mutated by caller")
	}
	wide := p.Clone()
	wide.Scope = "cluster"
	wide.Allowed = nil
	nested := WithReadPolicy(ctx, wide)
	if ContextReadPolicy(nested).Scope != "database" || CheckReadPolicy(nested, nil, StatTables, Args{}) == nil {
		t.Fatal("nested policy widened the context")
	}
	p.Allowed = []ID{}
	if p.Allows(StatDatabase) {
		t.Fatal("explicit empty policy allowed a read")
	}
}

func TestReadPolicyDeniesBeforePoolOrRetry(t *testing.T) {
	r := NewRunner(nil, Catalog(), nil, databasePolicy("test"))
	for _, run := range []func(context.Context, ID, Args) Result{r.Run, r.RunBackground} {
		res := run(context.Background(), ConnectionSaturation, Args{})
		if res.Reason != ReasonPolicyDenied || res.Status.Usable() || res.Timing.Attempts != 0 || len(res.Rows) != 0 {
			t.Fatalf("denial executed or appeared healthy: %+v", res)
		}
	}
}
