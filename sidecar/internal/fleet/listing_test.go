package fleet

import (
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// ux-10a: FleetListing is FleetStatus plus each database's host and
// execution mode, read under the same lock. No concurrency test here: the
// API package pages it during reloads (databases_list_sort_test.go).

func TestFleetListing_FacetsMatchConfigAndStatusIsUnchanged(t *testing.T) {
	m := NewManager(&config.Config{Mode: "fleet"})
	m.RegisterInstance(&DatabaseInstance{Name: "a", Config: config.DatabaseConfig{
		Name: "a", Host: "a.internal", ExecutionMode: "approval"},
		Status: &InstanceStatus{Connected: true}})
	m.RegisterInstance(&DatabaseInstance{Name: "b", Config: config.DatabaseConfig{
		Name: "b", Host: "b.internal"}, Status: &InstanceStatus{}})
	ov, facets := m.FleetListing()
	if len(facets) != 2 {
		t.Fatalf("facets = %v, want 2", facets)
	}
	if facets["a"] != (DatabaseFacets{Host: "a.internal", ExecutionMode: "approval"}) {
		t.Fatalf("facets[a] = %+v", facets["a"])
	}
	if facets["b"] != (DatabaseFacets{Host: "b.internal", ExecutionMode: "auto"}) {
		t.Fatalf("facets[b] = %+v, want the auto default for an unset mode", facets["b"])
	}
	want, _ := json.Marshal(m.FleetStatus())
	got, _ := json.Marshal(ov)
	if string(got) != string(want) {
		t.Fatalf("listing overview differs from FleetStatus:\n got %s\nwant %s", got, want)
	}
}

func TestFleetListing_EmptyAndReadView(t *testing.T) {
	m := NewManager(&config.Config{Mode: "fleet"})
	ov, facets := m.FleetListing()
	if len(ov.Databases) != 0 || ov.Databases == nil || len(facets) != 0 {
		t.Fatalf("empty fleet = %+v %v", ov, facets)
	}
	m.RegisterInstance(&DatabaseInstance{Name: "a", Config: config.DatabaseConfig{Name: "a"},
		Status: &InstanceStatus{}})
	m.RegisterInstance(&DatabaseInstance{Name: "b", Config: config.DatabaseConfig{Name: "b"},
		Status: &InstanceStatus{}})
	ov, facets = m.ReadView(map[string]bool{"b": true}).FleetListing()
	if len(ov.Databases) != 1 || ov.Databases[0].Name != "b" || len(facets) != 1 {
		t.Fatalf("read view listing = %+v %v, want only b", ov.Databases, facets)
	}
}
