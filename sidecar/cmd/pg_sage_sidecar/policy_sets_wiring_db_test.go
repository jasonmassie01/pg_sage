//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// fleetpol-01a on real PostgreSQL: a YAML fleet database's tags reach
// sage.databases, so it resolves to the prod set, and the sweep reverts an
// expired exception.
func TestPolicySetWiring_YAMLTagsSelectProdAndSweepReverts(t *testing.T) {
	pool := policyCeilingPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE sage.policy_exceptions;
		DELETE FROM sage.policy WHERE set_name IS NOT NULL OR database_id IS NOT NULL;
		DELETE FROM sage.databases WHERE name = 'orders-ps'`); err != nil {
		t.Fatal(err)
	}
	db := config.DatabaseConfig{Name: "orders-ps", Host: "h", Port: 5432, Database: "d",
		User: "u", SSLMode: "disable", MaxConnections: 2, Tags: []string{"tier=prod"}}
	id, err := upsertFleetDatabase(ctx, pool, db, "observation")
	if err != nil {
		t.Fatal(err)
	}
	id64 := int64(id)
	store := policy.NewStore(pool)
	current, err := store.Bootstrap(ctx, policy.Scope{DatabaseID: &id64}, "unattended",
		"bootstrap")
	if err != nil || current.Set != "prod" {
		t.Fatalf("a tier=prod fleet database resolved to set %q (%v), want prod",
			current.Set, err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := store.SetException(ctx, policy.ExceptionRequest{DatabaseID: id64,
		Actor: "dba", Reason: "batch window", ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	later := store.WithClock(func() time.Time { return expires })
	sweepPolicyExceptions(ctx, later)
	list, err := store.Exceptions(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("exceptions after the sweep = %+v (%v), want none", list, err)
	}
	after, err := store.Current(ctx, policy.Scope{DatabaseID: &id64})
	if err != nil || after.Set != "prod" {
		t.Fatalf("after the sweep the database resolved to %q (%v), want prod", after.Set, err)
	}
}
