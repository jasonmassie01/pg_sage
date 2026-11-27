package main

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleet-01: meta-DB registrations carry tags as a map; the fleet runtime and
// the fleet-learning tenant boundary read config.DatabaseConfig.Tags.

func TestRecordTagsSortedKeyValue(t *testing.T) {
	got := recordTags(map[string]string{"tier": "prod", "tenant": "acme"})
	want := []string{"tenant=acme", "tier=prod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recordTags = %#v, want %#v", got, want)
	}
	if b := fleetlearn.Boundary(got); b != "tenant:acme" {
		t.Fatalf("Boundary(%v) = %q, want tenant:acme", got, b)
	}
}

func TestRecordTagsNilAndEmptyAreNil(t *testing.T) {
	if got := recordTags(nil); got != nil {
		t.Fatalf("recordTags(nil) = %#v, want nil", got)
	}
	if got := recordTags(map[string]string{}); got != nil {
		t.Fatalf("recordTags({}) = %#v, want nil", got)
	}
}

func TestRecordTagsStableOrder(t *testing.T) {
	tags := map[string]string{"z": "1", "tenant": "acme", "b": "2", "a": "3",
		"region": "eu", "tier": "prod", "team": "pay", "Tenant": "upper"}
	first := recordTags(tags)
	want := []string{"Tenant=upper", "a=3", "b=2", "region=eu", "team=pay",
		"tenant=acme", "tier=prod", "z=1"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("recordTags = %#v, want byte-order sorted %#v", first, want)
	}
	for i := 0; i < 100; i++ {
		if got := recordTags(tags); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: recordTags = %#v, want %#v", i, got, first)
		}
	}
}

func TestRecordTagsSeparatorInKeyAndEmptyValue(t *testing.T) {
	got := recordTags(map[string]string{"a=b": "c"})
	if !reflect.DeepEqual(got, []string{"a=b=c"}) {
		t.Fatalf("key with '=' = %#v, want one tag a=b=c", got)
	}
	empty := recordTags(map[string]string{"tenant": ""})
	if !reflect.DeepEqual(empty, []string{"tenant="}) {
		t.Fatalf("empty value = %#v, want [tenant=]", empty)
	}
	if b := fleetlearn.Boundary(empty); b != "" {
		t.Fatalf("an empty tenant must not form a boundary, got %q", b)
	}
	// A key that merely contains "tenant=acme" must not join tenant acme.
	forged := recordTags(map[string]string{"tenant=acme": ""})
	if b := fleetlearn.Boundary(forged); b == "tenant:acme" {
		t.Fatalf("key %v impersonates tenant acme", forged)
	}
}

func TestStoreRecordToDBConfigCarriesTags(t *testing.T) {
	rec := store.DatabaseRecord{ID: 7, Name: "orders", Host: "h", Port: 5433,
		DatabaseName: "app", Username: "u", SSLMode: "require", MaxConnections: 4,
		Tags:       map[string]string{"tenant": "acme", "tier": "prod"},
		TrustLevel: "advisory", ExecutionMode: "manual"}
	got := storeRecordToDBConfig(rec)
	want := config.DatabaseConfig{Name: "orders", Host: "h", Port: 5433,
		Database: "app", User: "u", SSLMode: "require", MaxConnections: 4,
		Tags:       []string{"tenant=acme", "tier=prod"},
		TrustLevel: "advisory", TrustLevelExplicit: true, ExecutionMode: "manual"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("storeRecordToDBConfig = %+v\nwant %+v", got, want)
	}
	rec.Tags = map[string]string{}
	if got := storeRecordToDBConfig(rec); got.Tags != nil {
		t.Fatalf("a record without tags gives Tags %#v, want nil", got.Tags)
	}
}

// connectedMetaInstance is a running meta-DB instance: a lazy pool (it never
// dials here) and, when withExecutor, an executor holding rec's policy.
func connectedMetaInstance(
	t *testing.T, rec store.DatabaseRecord, withExecutor bool,
) (*fleet.DatabaseInstance, string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage@127.0.0.1:1/app?sslmode=disable")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	inst := &fleet.DatabaseInstance{Name: rec.Name, DatabaseID: rec.ID, Pool: pool,
		Config: storeRecordToDBConfig(rec), Status: &fleet.InstanceStatus{}}
	if withExecutor {
		inst.Executor = executor.New(nil, config.DefaultConfig(), time.Now(), nil)
		if err := inst.Executor.SetTrustLevel(rec.TrustLevel); err != nil {
			t.Fatalf("set trust: %v", err)
		}
		inst.Executor.SetExecutionMode(resolveExecMode(rec))
	}
	return inst, pool.Config().ConnString()
}

func taggedRecord(tags map[string]string) store.DatabaseRecord {
	return store.DatabaseRecord{ID: 3, Name: "orders", Host: "127.0.0.1", Port: 1,
		DatabaseName: "app", Username: "sage", MaxConnections: 2, Enabled: true,
		Tags: tags, TrustLevel: "observation", ExecutionMode: "auto"}
}

func TestMetaChangeKindTagOnlyEditIsUpdate(t *testing.T) {
	for _, withExecutor := range []bool{true, false} {
		inst, connStr := connectedMetaInstance(t,
			taggedRecord(map[string]string{"tenant": "acme"}), withExecutor)
		edited := taggedRecord(map[string]string{"tenant": "globex"})
		if got := metaChangeKind(edited, inst, connStr); got != "updated" {
			t.Fatalf("executor=%v: tag-only edit kind = %q, want updated", withExecutor, got)
		}
		untagged := taggedRecord(map[string]string{})
		if got := metaChangeKind(untagged, inst, connStr); got != "updated" {
			t.Fatalf("executor=%v: removing all tags kind = %q, want updated",
				withExecutor, got)
		}
	}
}

func TestMetaChangeKindSameTagsAnyOrderIsUnchanged(t *testing.T) {
	a := map[string]string{}
	a["tenant"], a["tier"], a["region"] = "acme", "prod", "eu"
	b := map[string]string{}
	b["region"], b["tier"], b["tenant"] = "eu", "prod", "acme"
	for _, withExecutor := range []bool{true, false} {
		inst, connStr := connectedMetaInstance(t, taggedRecord(a), withExecutor)
		if got := metaChangeKind(taggedRecord(b), inst, connStr); got != "" {
			t.Fatalf("executor=%v: same tags kind = %q, want unchanged", withExecutor, got)
		}
	}
	// nil and empty tag sets are the same set.
	inst, connStr := connectedMetaInstance(t, taggedRecord(nil), true)
	inst.Config.Tags = []string{}
	if got := metaChangeKind(taggedRecord(map[string]string{}), inst, connStr); got != "" {
		t.Fatalf("empty vs nil tags kind = %q, want unchanged", got)
	}
}

func TestMetaChangeKindKeepsRebuildAndPolicyRules(t *testing.T) {
	rec := taggedRecord(map[string]string{"tenant": "acme"})
	inst, connStr := connectedMetaInstance(t, rec, true)
	if got := metaChangeKind(rec, nil, connStr); got != "added" {
		t.Fatalf("no runtime kind = %q, want added", got)
	}
	if got := metaChangeKind(rec, inst, connStr+"&application_name=x"); got != "rebuilt" {
		t.Fatalf("connection change kind = %q, want rebuilt", got)
	}
	policy := rec
	policy.TrustLevel = "advisory"
	if got := metaChangeKind(policy, inst, connStr); got != "updated" {
		t.Fatalf("trust change kind = %q, want updated", got)
	}
	// A failed placeholder is rebuilt (not updated) when its record changes,
	// tags included: the reconnect loop owns its runtime.
	placeholder := failedStoreInstance(rec, "down")
	edited := taggedRecord(map[string]string{"tenant": "globex"})
	if got := metaChangeKind(edited, placeholder, connStr); got != "rebuilt" {
		t.Fatalf("placeholder tag edit kind = %q, want rebuilt", got)
	}
	if got := metaChangeKind(rec, placeholder, connStr); got != "" {
		t.Fatalf("unchanged placeholder kind = %q, want unchanged", got)
	}
}

// applyTagEdit runs applyMetaPolicy under a lifecycle reservation.
func applyTagEdit(t *testing.T, mgr *fleet.DatabaseManager,
	inst *fleet.DatabaseInstance, rec store.DatabaseRecord) error {
	t.Helper()
	return mgr.WithLifecycle(context.Background(), func(op *fleet.LifecycleMutation) error {
		return applyMetaPolicy(op, rec, inst)
	})
}

func TestApplyMetaPolicyTagEditReachesFleetLearning(t *testing.T) {
	for _, withExecutor := range []bool{true, false} {
		mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
		inst, _ := connectedMetaInstance(t,
			taggedRecord(map[string]string{"tenant": "acme"}), withExecutor)
		mgr.RegisterInstance(inst)
		if got := fleetLearningSources(mgr)(); len(got) != 1 ||
			got[0].Boundary != "tenant:acme" {
			t.Fatalf("executor=%v: sources before = %+v", withExecutor, got)
		}
		edited := taggedRecord(map[string]string{"tenant": "globex"})
		if err := applyTagEdit(t, mgr, inst, edited); err != nil {
			t.Fatalf("executor=%v: apply tag edit: %v", withExecutor, err)
		}
		if mgr.GetInstance("orders") != inst {
			t.Fatalf("executor=%v: a tag edit replaced the runtime", withExecutor)
		}
		got := fleetLearningSources(mgr)()
		if len(got) != 1 || got[0].Boundary != "tenant:globex" || got[0].Pool != inst.Pool {
			t.Fatalf("executor=%v: sources after = %+v, want tenant:globex", withExecutor, got)
		}
		if withExecutor && inst.Executor.TrustLevel() != "observation" {
			t.Fatalf("a tag edit changed trust to %s", inst.Executor.TrustLevel())
		}
	}
}

func TestApplyMetaPolicyRejectsStaleGeneration(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	rec := taggedRecord(map[string]string{"tenant": "acme"})
	stale, _ := connectedMetaInstance(t, rec, false)
	current, _ := connectedMetaInstance(t, rec, false)
	mgr.RegisterInstance(current)
	err := applyTagEdit(t, mgr, stale, taggedRecord(map[string]string{"tenant": "globex"}))
	if err == nil {
		t.Fatal("a tag edit against a replaced generation must fail")
	}
	if b := fleetLearningSources(mgr)()[0].Boundary; b != "tenant:acme" {
		t.Fatalf("stale edit leaked into the running generation: boundary %q", b)
	}
}

// TestFleetLearningSourcesRaceWithTagEdits: CI runs this under -race; the
// boundary read must go through the manager lock (InstanceConfigs).
func TestFleetLearningSourcesRaceWithTagEdits(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	inst, _ := connectedMetaInstance(t, taggedRecord(map[string]string{"tenant": "acme"}), true)
	mgr.RegisterInstance(inst)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			tenant := map[bool]string{true: "acme", false: "globex"}[i%2 == 0]
			if err := applyTagEdit(t, mgr, inst,
				taggedRecord(map[string]string{"tenant": tenant})); err != nil {
				t.Errorf("edit %d: %v", i, err)
				return
			}
		}
	}()
	seen := map[string]bool{}
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			for _, src := range fleetLearningSources(mgr)() {
				seen[src.Boundary] = true
			}
		}
	}()
	wg.Wait()
	for b := range seen {
		if b != "tenant:acme" && b != "tenant:globex" {
			t.Fatalf("observed torn boundary %q", b)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the reader saw no sources")
	}
}

func TestMetaBoundarySummary(t *testing.T) {
	recs := []store.DatabaseRecord{
		taggedRecord(map[string]string{"tenant": "acme"}),
		taggedRecord(map[string]string{"tenant": "Acme", "tier": "prod"}),
		taggedRecord(map[string]string{"tenant": "globex"}),
		taggedRecord(nil),
		taggedRecord(map[string]string{"tenant": ""}),
	}
	got := metaBoundarySummary(recs)
	if want := "shared=2 tenant:acme=2 tenant:globex=1"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got := metaBoundarySummary(nil); got != "none" {
		t.Fatalf("empty summary = %q, want none", got)
	}
}
