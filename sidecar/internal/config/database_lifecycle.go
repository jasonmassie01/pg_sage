package config

import (
	"reflect"
	"sort"
	"strings"
	"sync"
)

// FleetDatabasesOwner reconciles the YAML fleet's databases at runtime: it
// adds, removes and rebuilds per-database runtimes and applies per-database
// policy in place. Modes without it (standalone, meta-db) fail closed to
// restart for the keys it owns.
const FleetDatabasesOwner = "fleet_databases"

const databaseFieldPrefix = "databases[]."

// connectionOptionsField names the private DSN transport options (for
// example connect_timeout) carried from a secret DSN.
const connectionOptionsField = "connection_options"

// liveDatabaseFields change a running database's policy in place: the
// executor and the instance metadata adopt them without a rebuild. Every
// other per-database field reconnects or rebuilds that database's runtime.
var liveDatabaseFields = map[string]bool{
	"trust_level": true, "execution_mode": true,
	"executor_enabled": true, "tags": true,
}

type databaseFieldEntry struct {
	FieldLifecycle
	index []int
}

var (
	databaseFieldsOnce  sync.Once
	databaseFieldByPath map[string]databaseFieldEntry
	databaseFieldPaths  []string
)

func initializeDatabaseFields() {
	databaseFieldsOnce.Do(func() {
		databaseFieldByPath = map[string]databaseFieldEntry{}
		walkDatabaseFields(reflect.TypeOf(DatabaseConfig{}), nil, "")
		for path := range databaseFieldByPath {
			databaseFieldPaths = append(databaseFieldPaths, path)
		}
		sort.Strings(databaseFieldPaths)
	})
}

func walkDatabaseFields(typ reflect.Type, parent []int, prefix string) {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		index := append(append([]int(nil), parent...), i)
		if field.Type.Kind() == reflect.Struct {
			walkDatabaseFields(field.Type, index, prefix+name+".")
			continue
		}
		path := prefix + name
		lifecycle := LifecycleReconfigure
		if liveDatabaseFields[path] {
			lifecycle = LifecycleLivePolicy
		}
		databaseFieldByPath[path] = databaseFieldEntry{
			FieldLifecycle: FieldLifecycle{
				Path: databaseFieldPrefix + path, Lifecycle: lifecycle,
				Owner: FleetDatabasesOwner,
			},
			index: index,
		}
	}
}

// LookupDatabaseFieldLifecycle classifies one per-database field, named as
// its YAML path below a databases[] entry (for example "verify.io_capacity").
func LookupDatabaseFieldLifecycle(field string) (FieldLifecycle, bool) {
	initializeDatabaseFields()
	entry, ok := databaseFieldByPath[field]
	return entry.FieldLifecycle, ok
}

// DatabaseFieldLifecycles returns every per-database field, sorted.
func DatabaseFieldLifecycles() []FieldLifecycle {
	initializeDatabaseFields()
	fields := make([]FieldLifecycle, 0, len(databaseFieldPaths))
	for _, path := range databaseFieldPaths {
		fields = append(fields, databaseFieldByPath[path].FieldLifecycle)
	}
	return fields
}

// DatabaseChange is the difference between two configs of one database.
type DatabaseChange struct {
	// Fields are the changed per-database fields, sorted.
	Fields []string
	// Rebuild is true when any changed field cannot apply in place.
	Rebuild bool
}

// Changed reports whether anything differs.
func (c DatabaseChange) Changed() bool { return len(c.Fields) > 0 }

// ClassifyDatabaseChange compares two configs of the same database. Gates
// compare by effective value (an unset gate means enabled), and a trust
// level that switches between explicit and inherited is a policy change.
func ClassifyDatabaseChange(old, updated DatabaseConfig) DatabaseChange {
	initializeDatabaseFields()
	var change DatabaseChange
	oldValue, newValue := reflect.ValueOf(old), reflect.ValueOf(updated)
	for _, path := range databaseFieldPaths {
		entry := databaseFieldByPath[path]
		if !databaseFieldChanged(path, entry.index, old, updated, oldValue, newValue) {
			continue
		}
		change.Fields = append(change.Fields, path)
		change.Rebuild = change.Rebuild || entry.Lifecycle != LifecycleLivePolicy
	}
	if old.connectionOptions != updated.connectionOptions {
		change.Fields = append(change.Fields, connectionOptionsField)
		change.Rebuild = true
	}
	sort.Strings(change.Fields)
	return change
}

func databaseFieldChanged(
	path string, index []int, old, updated DatabaseConfig,
	oldValue, newValue reflect.Value,
) bool {
	switch path {
	case "executor_enabled":
		return old.IsExecutorEnabled() != updated.IsExecutorEnabled()
	case "llm_enabled":
		return old.IsLLMEnabled() != updated.IsLLMEnabled()
	case "trust_level":
		return old.TrustLevel != updated.TrustLevel ||
			old.TrustLevelExplicit != updated.TrustLevelExplicit
	case "tags":
		return !sameTags(old.Tags, updated.Tags)
	}
	return !reflect.DeepEqual(
		oldValue.FieldByIndex(index).Interface(),
		newValue.FieldByIndex(index).Interface(),
	)
}

// sameTags treats nil and empty tag lists as equal.
func sameTags(left, right []string) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	return reflect.DeepEqual(left, right)
}
