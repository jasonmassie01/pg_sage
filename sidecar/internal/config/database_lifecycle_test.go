package config

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// No concurrent access tests: the per-database registry is built once
// under sync.Once and every function here is a pure read of it.

func TestFleetDatabaseKeysReconfigureThroughFleetOwner(t *testing.T) {
	for _, path := range []string{
		"databases", "defaults.trust_level", "defaults.execution_mode",
		"defaults.max_connections", "defaults.collector_interval_seconds",
		"defaults.analyzer_interval_seconds",
	} {
		meta, ok := LookupFieldLifecycle(path)
		if !ok {
			t.Fatalf("%s is not registered", path)
		}
		if meta.Lifecycle != LifecycleReconfigure || meta.Owner != FleetDatabasesOwner {
			t.Errorf("%s = %s/%q, want reconfigure/%s",
				path, meta.Lifecycle, meta.Owner, FleetDatabasesOwner)
		}
	}
	// Without the owner (standalone, meta-db) the controller fails closed.
	controller := NewConfigController(DefaultConfig(), nil)
	if got, _ := controller.EffectiveLifecycle("databases"); got != LifecycleRestart {
		t.Fatalf("unowned databases lifecycle = %q, want restart", got)
	}
}

func TestDatabaseFieldRegistryCoversEveryYAMLField(t *testing.T) {
	want := databaseYAMLPaths(reflect.TypeOf(DatabaseConfig{}), "")
	got := map[string]bool{}
	for _, field := range DatabaseFieldLifecycles() {
		if !strings.HasPrefix(field.Path, "databases[].") {
			t.Errorf("per-database path %q lacks the databases[]. prefix", field.Path)
		}
		got[strings.TrimPrefix(field.Path, "databases[].")] = true
		if field.Owner != FleetDatabasesOwner {
			t.Errorf("%s owner = %q", field.Path, field.Owner)
		}
	}
	for _, path := range want {
		if !got[path] {
			t.Errorf("DatabaseConfig yaml field %q has no lifecycle", path)
		}
	}
	if len(got) != len(want) {
		t.Errorf("registry has %d fields, DatabaseConfig has %d", len(got), len(want))
	}
}

func databaseYAMLPaths(typ reflect.Type, prefix string) []string {
	var paths []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if field.Type.Kind() == reflect.Struct {
			paths = append(paths, databaseYAMLPaths(field.Type, prefix+name+".")...)
			continue
		}
		paths = append(paths, prefix+name)
	}
	return paths
}

func TestDatabaseFieldLifecycleClasses(t *testing.T) {
	hot := []string{"trust_level", "execution_mode", "executor_enabled", "tags"}
	rebuild := []string{
		"name", "host", "port", "user", "password", "database", "sslmode",
		"max_connections", "llm_enabled", "collector_interval_seconds",
		"analyzer_interval_seconds", "verify.io_capacity",
	}
	for _, field := range hot {
		assertDatabaseFieldLifecycle(t, field, LifecycleLivePolicy)
	}
	for _, field := range rebuild {
		assertDatabaseFieldLifecycle(t, field, LifecycleReconfigure)
	}
	for _, unknown := range []string{"", "nope", "verify", "databases[].host"} {
		if meta, ok := LookupDatabaseFieldLifecycle(unknown); ok {
			t.Errorf("unknown field %q classified as %+v", unknown, meta)
		}
	}
}

func assertDatabaseFieldLifecycle(t *testing.T, field string, want ConfigLifecycle) {
	t.Helper()
	meta, ok := LookupDatabaseFieldLifecycle(field)
	if !ok {
		t.Fatalf("%s is not registered", field)
	}
	if meta.Lifecycle != want || meta.Path != "databases[]."+field {
		t.Errorf("%s = %+v, want %s", field, meta, want)
	}
}

func baseDatabase() DatabaseConfig {
	enabled := true
	return DatabaseConfig{
		Name: "orders", Host: "db1", Port: 5432, User: "sage", Password: "pw",
		Database: "orders", SSLMode: "disable", MaxConnections: 4,
		Tags: []string{"prod"}, TrustLevel: "advisory", TrustLevelExplicit: true,
		ExecutionMode: "approval", ExecutorEnabled: &enabled,
	}
}

func TestClassifyDatabaseChangeUnchanged(t *testing.T) {
	change := ClassifyDatabaseChange(baseDatabase(), baseDatabase())
	if change.Changed() || change.Rebuild || len(change.Fields) != 0 {
		t.Fatalf("identical configs classified as %+v", change)
	}
	if ClassifyDatabaseChange(DatabaseConfig{}, DatabaseConfig{}).Changed() {
		t.Fatal("zero configs classified as changed")
	}
}

func TestClassifyDatabaseChangeHotFieldsApplyInPlace(t *testing.T) {
	updated := baseDatabase()
	updated.TrustLevel = "autonomous"
	updated.ExecutionMode = "auto"
	disabled := false
	updated.ExecutorEnabled = &disabled
	updated.Tags = []string{"prod", "payments"}
	change := ClassifyDatabaseChange(baseDatabase(), updated)
	want := []string{"execution_mode", "executor_enabled", "tags", "trust_level"}
	if change.Rebuild || !reflect.DeepEqual(change.Fields, want) {
		t.Fatalf("hot change = %+v, want fields %v without rebuild", change, want)
	}
}

func TestClassifyDatabaseChangeConnectionFieldsRebuild(t *testing.T) {
	cases := map[string]func(*DatabaseConfig){
		"host":                       func(d *DatabaseConfig) { d.Host = "db2" },
		"port":                       func(d *DatabaseConfig) { d.Port = 6432 },
		"password":                   func(d *DatabaseConfig) { d.Password = "rotated" },
		"max_connections":            func(d *DatabaseConfig) { d.MaxConnections = 9 },
		"collector_interval_seconds": func(d *DatabaseConfig) { d.CollectorIntervalSeconds = 30 },
		"verify.io_capacity": func(d *DatabaseConfig) {
			d.Verify.IOCapacity = &IOCapacityConfig{}
		},
	}
	for field, mutate := range cases {
		updated := baseDatabase()
		mutate(&updated)
		change := ClassifyDatabaseChange(baseDatabase(), updated)
		if !change.Rebuild || !reflect.DeepEqual(change.Fields, []string{field}) {
			t.Errorf("%s change = %+v, want rebuild of [%s]", field, change, field)
		}
	}
}

func TestClassifyDatabaseChangeMixedRebuildsAndListsAllFields(t *testing.T) {
	updated := baseDatabase()
	updated.Host = "db2"
	updated.TrustLevel = "observation"
	change := ClassifyDatabaseChange(baseDatabase(), updated)
	if !change.Rebuild || !reflect.DeepEqual(change.Fields, []string{"host", "trust_level"}) {
		t.Fatalf("mixed change = %+v", change)
	}
}

func TestClassifyDatabaseChangeComparesEffectiveGates(t *testing.T) {
	explicitTrue := true
	withNil := baseDatabase()
	withNil.ExecutorEnabled, withNil.LLMEnabled = nil, nil
	withTrue := baseDatabase()
	withTrue.ExecutorEnabled, withTrue.LLMEnabled = &explicitTrue, &explicitTrue
	if change := ClassifyDatabaseChange(withNil, withTrue); change.Changed() {
		t.Fatalf("nil and true gates are the same effective value: %+v", change)
	}
	explicitFalse := false
	withFalse := baseDatabase()
	withFalse.LLMEnabled = &explicitFalse
	change := ClassifyDatabaseChange(withNil, withFalse)
	if !change.Rebuild || !reflect.DeepEqual(change.Fields, []string{"llm_enabled"}) {
		t.Fatalf("llm_enabled false change = %+v", change)
	}
}

func TestClassifyDatabaseChangeTrustSourceAndConnectionOptions(t *testing.T) {
	inherited := baseDatabase()
	inherited.TrustLevelExplicit = false
	change := ClassifyDatabaseChange(baseDatabase(), inherited)
	if change.Rebuild || !reflect.DeepEqual(change.Fields, []string{"trust_level"}) {
		t.Fatalf("trust source change = %+v, want hot trust_level", change)
	}
	withOptions := baseDatabase()
	withOptions.SetRuntimeConnectionOptions(url.Values{"connect_timeout": {"7"}})
	change = ClassifyDatabaseChange(baseDatabase(), withOptions)
	if !change.Rebuild || !reflect.DeepEqual(change.Fields, []string{"connection_options"}) {
		t.Fatalf("connection option change = %+v", change)
	}
}

func TestFleetOwnerAppliesDatabasesInsteadOfPendingRestart(t *testing.T) {
	owner := &ownerStub{name: FleetDatabasesOwner}
	initial := DefaultConfig()
	initial.Mode = "fleet"
	initial.Databases = []DatabaseConfig{baseDatabase()}
	controller := NewConfigController(initial, nil, owner)
	candidate := controller.Active().Config
	added := baseDatabase()
	added.Name = "billing"
	candidate.Databases = append(candidate.Databases, added)
	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !containsPath(result.Applied, "databases") || len(result.PendingRestart) != 0 {
		t.Fatalf("result = %+v, want databases applied", result)
	}
	if owner.prepared == nil || owner.prepared.commit != 1 || owner.prepared.drain != 1 {
		t.Fatalf("fleet owner was not prepared, committed and drained: %+v", owner.prepared)
	}
	if got := len(controller.Active().Config.Databases); got != 2 {
		t.Fatalf("active databases = %d, want 2", got)
	}
}

func TestFleetOwnerPrepareFailureRejectsWholeReload(t *testing.T) {
	sentinel := errors.New("candidate unreachable")
	owner := &ownerStub{name: FleetDatabasesOwner, prepareErr: sentinel}
	initial := DefaultConfig()
	initial.Mode = "fleet"
	initial.Databases = []DatabaseConfig{baseDatabase()}
	controller := NewConfigController(initial, nil, owner)
	candidate := controller.Active().Config
	candidate.Databases[0].Port = 1
	candidate.Defaults.TrustLevel = "observation"
	if _, err := controller.Apply(context.Background(), 1, candidate); !errors.Is(err, sentinel) {
		t.Fatalf("apply error = %v, want the owner's prepare error", err)
	}
	assertGenerations(t, controller, 1, 1)
	if got := controller.Active().Config.Databases[0].Port; got != 5432 {
		t.Fatalf("rejected reload changed the active port to %d", got)
	}
}

func TestLifecycleReferenceListsPerDatabaseFields(t *testing.T) {
	doc := ConfigLifecycleMarkdown()
	for _, row := range []string{
		"| `databases` | `reconfigure` | `fleet_databases` |",
		"| `databases[].trust_level` | `live_policy` | `fleet_databases` |",
		"| `databases[].host` | `reconfigure` | `fleet_databases` |",
		"| `defaults.trust_level` | `reconfigure` | `fleet_databases` |",
	} {
		if !strings.Contains(doc, row) {
			t.Errorf("lifecycle reference omits %q", row)
		}
	}
	if strings.Contains(doc, "lifecycle_api") {
		t.Error("lifecycle reference still documents the removed lifecycle_api class")
	}
}
