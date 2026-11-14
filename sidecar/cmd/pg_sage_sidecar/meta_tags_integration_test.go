package main

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/fleetlearn"
)

// fleet-01: meta-DB tags reach the running fleet and the fleet-learning
// tenant boundary, on real PostgreSQL.

// setRecordTags rewrites a registration's tags out of band, the way another
// replica or an operator's SQL would.
func setRecordTags(t *testing.T, env *metaReconcileEnv, id int, tagsJSON string) {
	t.Helper()
	if _, err := env.state.Pool.Exec(context.Background(),
		`UPDATE sage.databases SET tags = $1::jsonb WHERE id = $2`, tagsJSON, id); err != nil {
		t.Fatalf("set tags of %d: %v", id, err)
	}
}

func TestMetaReconcileAppliesTagEditInPlace(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "tagged")
	setRecordTags(t, env, rec.ID, `{"tenant":"acme","tier":"prod"}`)
	env.reconcile(t)
	old := fleetMgr.GetInstance("tagged")
	if old == nil || old.Pool == nil {
		t.Fatal("tagged database is not running")
	}
	if !reflect.DeepEqual(old.Config.Tags, []string{"tenant=acme", "tier=prod"}) {
		t.Fatalf("running tags = %#v, want the registration's tags", old.Config.Tags)
	}
	if again := env.reconcile(t); len(again.Updated)+len(again.Rebuilt) != 0 {
		t.Fatalf("an unchanged tag set was reapplied: %+v", again)
	}
	setRecordTags(t, env, rec.ID, `{"tenant":"globex","tier":"prod"}`)
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Updated, []string{"tagged"}) || len(report.Rebuilt) != 0 {
		t.Fatalf("report = %+v, want an in-place update", report)
	}
	inst := fleetMgr.GetInstance("tagged")
	if inst != old || poolClosed(old.Pool) {
		t.Fatal("a tag-only change rebuilt the runtime")
	}
	srcs := fleetLearningSources(fleetMgr)()
	if len(srcs) != 1 || srcs[0].Boundary != "tenant:globex" {
		t.Fatalf("sources = %+v, want tagged in tenant:globex", srcs)
	}
	if inst.Executor != nil && inst.Executor.TrustLevel() != "observation" {
		t.Fatalf("a tag edit changed trust to %s", inst.Executor.TrustLevel())
	}
	status := fleetMgr.FleetStatus()
	if len(status.Databases) != 1 ||
		!reflect.DeepEqual(status.Databases[0].Tags, []string{"tenant=globex", "tier=prod"}) {
		t.Fatalf("GET /api/v1/databases tags = %+v", status.Databases)
	}
}

// lookalikeSchemaSQL is the tenants' application schema. It runs on the
// fleet instance's pool, pinned to pg_catalog, pg_temp, so it names public.
const lookalikeSchemaSQL = `
CREATE TABLE public.customers (id bigint PRIMARY KEY, email text NOT NULL, created timestamptz);
CREATE TABLE public.orders (id bigint PRIMARY KEY, customer_id bigint, total numeric(12,2),
	status text, placed timestamptz);
CREATE INDEX ON public.orders (customer_id);
CREATE UNIQUE INDEX ON public.customers (email)`

// tenantFleet registers one same-shaped database per name, tagged with its
// tenant, and fingerprints the fleet the way the leader's cycle does.
func tenantFleet(t *testing.T, tenants map[string]string) *fleetlearn.Service {
	t.Helper()
	env := newMetaReconcileEnv(t)
	for name, tenant := range tenants {
		rec := env.record(t, name)
		setRecordTags(t, env, rec.ID, `{"tenant":"`+tenant+`"}`)
	}
	// The reconnect loop retries an item whose build timed out; so does the
	// test, on a loaded machine, before it requires every runtime.
	for pass := 0; pass < 4 && len(fleetMgr.Instances()) < len(tenants); pass++ {
		if report := env.reconcile(t); len(report.Errors) > 0 {
			t.Logf("reconcile pass %d: %v", pass, report.Errors)
		}
	}
	for name := range tenants {
		inst := fleetMgr.GetInstance(name)
		if inst == nil || inst.Pool == nil {
			t.Fatalf("%s is not running", name)
		}
		if _, err := inst.Pool.Exec(context.Background(), lookalikeSchemaSQL); err != nil {
			t.Fatalf("schema on %s: %v", name, err)
		}
	}
	svc := fleetlearn.NewService(fleetlearn.NewStore(env.state.Pool, "meta"),
		fleetLearningSources(fleetMgr), fleetlearn.Settings{}, nil)
	res, err := svc.RunCycle(context.Background(), fleetlearn.Fence{})
	if err != nil || res.Databases != len(tenants) || len(res.Failed) != 0 {
		t.Fatalf("cycle = %+v err=%v, want %d fingerprinted", res, err, len(tenants))
	}
	return svc
}

func lookalikeNames(t *testing.T, svc *fleetlearn.Service, db string) []string {
	t.Helper()
	looks, err := svc.LookAlikes(context.Background(), db)
	if err != nil {
		t.Fatalf("look-alikes of %s: %v", db, err)
	}
	names := []string{}
	for _, l := range looks {
		names = append(names, l.Database)
	}
	sort.Strings(names)
	return names
}

func TestMetaTenantTagsSeparateLookalikes(t *testing.T) {
	svc := tenantFleet(t, map[string]string{
		"acme_one": "acme", "acme_two": "acme", "globex_one": "globex"})
	if got := lookalikeNames(t, svc, "acme_one"); !reflect.DeepEqual(got,
		[]string{"acme_two"}) {
		t.Fatalf("acme_one look-alikes = %v, want only acme_two", got)
	}
	if got := lookalikeNames(t, svc, "globex_one"); len(got) != 0 {
		t.Fatalf("globex_one look-alikes = %v, want none across tenants", got)
	}
}

func TestMetaSameTenantTagsShareLookalikes(t *testing.T) {
	svc := tenantFleet(t, map[string]string{"left": "acme", "right": "ACME"})
	if got := lookalikeNames(t, svc, "left"); !reflect.DeepEqual(got, []string{"right"}) {
		t.Fatalf("left look-alikes = %v, want right (same tenant)", got)
	}
}
