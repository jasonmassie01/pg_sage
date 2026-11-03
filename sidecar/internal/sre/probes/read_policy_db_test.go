package probes

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReadPolicyTwoDatabasesNoForeignObservation(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("sage_scope_foreign_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	fc := pool.Config().Copy()
	fc.ConnConfig.Database = name
	foreign, err := pgxpool.NewWithConfig(ctx, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { foreign.Close(); _, _ = pool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()) })
	if _, err := foreign.Exec(ctx, "CREATE TABLE policy_private_marker(secret text); INSERT INTO policy_private_marker VALUES ('foreign-marker-must-not-leak')"); err != nil {
		t.Fatal(err)
	}
	policy := databasePolicy(pool.Config().ConnConfig.Database)
	policy.Binding = PoolBinding(pool)
	r := NewRunner(pool, Catalog(), nil, policy)
	res := r.Run(ctx, StatDatabase, Args{})
	if res.Status != StatusOK || len(res.Rows) != 1 || res.Rows[0]["database"] != policy.Database {
		t.Fatalf("local database: %+v", res)
	}
	// The same trusted policy may not be rebound to another physical database.
	wrong := NewRunner(foreign, Catalog(), nil, policy)
	for _, run := range []func(ID) Result{func(id ID) Result { return wrong.Run(ctx, id, Args{}) }, func(id ID) Result { return wrong.RunBackground(ctx, id, Args{}) }} {
		res := run(StatDatabase)
		if res.Reason != ReasonPolicyDenied || res.Timing.Attempts != 0 {
			t.Fatalf("wrong database observed: %+v", res)
		}
	}
	// A denied catalog source is refused before executing even trusted SQL that
	// would read a private marker. The foreign marker remains setup-only data.
	reg := mustTestRegistry(t, testSpec(ConnectionSaturation, "SELECT secret FROM policy_private_marker LIMIT $1"))
	foreignPolicy := databasePolicy(name)
	foreignPolicy.Binding = PoolBinding(foreign)
	denied := NewRunner(foreign, reg, nil, foreignPolicy).Run(ctx, ConnectionSaturation, Args{})
	if denied.Reason != ReasonPolicyDenied || denied.Timing.Attempts != 0 || len(denied.Rows) != 0 || strings.Contains(denied.Text(20), "foreign-marker") {
		t.Fatalf("foreign read executed: %+v", denied)
	}
	// Native local blockers remain usable with the database-safe variant.
	fixture := startChain(t, ctx, pool)
	graph := r.Run(ctx, LockGraph, Args{})
	if graph.Status != StatusOK || graph.Version != "v3_database" {
		t.Fatalf("local lock graph: %+v", graph)
	}
	var found bool
	for _, row := range graph.Rows {
		if row["blocker_pid"] == int64(fixture.holderPID) {
			found = true
		}
	}
	if !found {
		t.Fatal("database-safe graph lost the local root")
	}
}
