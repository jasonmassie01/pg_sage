package decommission

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// G1 drops the legacy tables by LegacyTables, so it must be exactly the 27
// tables the removed provisioner created (its schema is the fixture).
func TestLegacyTables_MatchTheRemovedSchema(t *testing.T) {
	ddl, err := os.ReadFile("testdata/legacy_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS sage\.([a-z_]+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(string(ddl), -1) {
		created = append(created, m[1])
	}
	want := slices.Clone(LegacyTables)
	slices.Sort(created)
	slices.Sort(want)
	if len(want) != 27 || !slices.Equal(created, want) {
		t.Fatalf("LegacyTables = %v\nschema creates %v", want, created)
	}
	if slices.Contains(LegacyTables, AckTable) {
		t.Fatal("the acknowledgement table must outlive the legacy drop")
	}
}

func TestRemovedRoutes_AreUniqueAndUnderTheRemovedPrefixes(t *testing.T) {
	seen := map[Route]bool{}
	for _, r := range RemovedRoutes {
		if seen[r] {
			t.Fatalf("route %v listed twice", r)
		}
		seen[r] = true
		if !strings.HasPrefix(r.Path, dbs) && !strings.HasPrefix(r.Path, agent) {
			t.Fatalf("route %v is outside the removed API", r)
		}
		if strings.HasPrefix(r.Path, InventoryPath) {
			t.Fatalf("the decommission route %v must not be listed as removed", r)
		}
	}
}
