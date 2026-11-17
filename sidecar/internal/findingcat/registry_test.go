package findingcat

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// No concurrent access tests: the registry is built at init and only read;
// All and the lookups return copies or fresh slices.

func TestRegistryIsValidAndSorted(t *testing.T) {
	if err := validate(declared()); err != nil {
		t.Fatalf("declared registry is invalid: %v", err)
	}
	all := All()
	if len(all) < 60 {
		t.Fatalf("registry has %d entries, want every producer's (60+)", len(all))
	}
	byName := func(x, y Category) int { return strings.Compare(x.Name, y.Name) }
	if !slices.IsSortedFunc(all, byName) {
		t.Fatal("All() is not sorted by name")
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	good := Category{Name: "x", Subsystem: "rules", Producer: "p", Severities: sevI,
		Summary: "s"}
	cases := map[string][]Category{
		"declared twice": {good, good},
		"no name":        {{Subsystem: "rules", Producer: "p", Severities: sevI, Summary: "s"}},
		"needs a subsystem": {{Name: "y", Producer: "p", Severities: sevI,
			Summary: "s"}},
		"unknown severity": {{Name: "y", Subsystem: "rules", Producer: "p",
			Severities: []string{"fatal"}, Summary: "s"}},
		"no severity": {{Name: "y", Subsystem: "rules", Producer: "p", Summary: "s"}},
		"own subsystem": {{Name: "y", Subsystem: "rules", AlsoIn: []string{"rules"},
			Producer: "p", Severities: sevI, Summary: "s"}},
	}
	for want, entries := range cases {
		err := validate(entries)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validate(%s) = %v, want an error containing %q", want, err, want)
		}
	}
	if err := validate(nil); err != nil {
		t.Errorf("validate(nil) = %v, want nil", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("mustBuild accepted a duplicate name")
		}
	}()
	mustBuild([]Category{good, good})
}

func TestLookupExactPrefixAndUnknown(t *testing.T) {
	cases := map[string]string{
		"missing_index":                   "optimizer",
		"stale_statistics":                "rules",
		"sage_self_cost":                  "rules",
		"forecast_wal_runway":             "forecaster",
		"forecast_never_declared":         "forecaster",
		"schema_lint:lint_no_primary_key": "schema_lint",
		"agent_posture:superuser":         "governance",
		"runaway_query":                   "query_tuning",
	}
	for name, want := range cases {
		c, ok := Lookup(name)
		if !ok || c.Subsystem != want {
			t.Errorf("Lookup(%q) = (%q, %v), want %q", name, c.Subsystem, ok, want)
		}
	}
	for _, name := range []string{"", "bogus", "schema_lint", "agent_posture", "advisor",
		"optimizer", "Missing_Index"} {
		if c, ok := Lookup(name); ok {
			t.Errorf("Lookup(%q) = %+v, want not found", name, c)
		}
	}
	if c, _ := Lookup("forecast_wal_runway"); c.Prefix || c.Producer != "runway" {
		t.Errorf("an exact entry must win over the forecast family: %+v", c)
	}
}

func TestBySubsystemSharedAndUnknown(t *testing.T) {
	for _, s := range []string{"rules", "query_tuning"} {
		if !slices.Contains(Names(BySubsystem(s)), "stale_statistics") {
			t.Errorf("%s does not list stale_statistics", s)
		}
	}
	if got := BySubsystem("bogus"); got != nil {
		t.Errorf("BySubsystem(bogus) = %v, want nil", got)
	}
	if got := BySubsystem(""); got != nil {
		t.Errorf("BySubsystem(\"\") = %v, want nil", got)
	}
	subs := Subsystems()
	for _, want := range []string{"advisor", "forecaster", "incident", "optimizer",
		"query_tuning", "rules", "schema_lint"} {
		if !slices.Contains(subs, want) {
			t.Errorf("Subsystems() = %v, misses %q", subs, want)
		}
	}
	if slices.Contains(subs, "migration_advisor") {
		t.Error("migration_advisor is reserved and must hold no category")
	}
}

func TestAllReturnsACopy(t *testing.T) {
	first := All()
	first[0].Name = "mutated"
	if All()[0].Name == "mutated" {
		t.Fatal("All() exposes the registry's backing array")
	}
	if got := ByProducer("nobody"); got != nil {
		t.Errorf("ByProducer(nobody) = %v, want nil", got)
	}
}

// retired are category strings the code still names but no producer
// emits; a migration removes their rows.
var retired = map[string]string{
	"forecast_sequence_exhaustion": "schema/sequence_forecast_retire_migration.go",
}

var (
	categoryLiteral  = regexp.MustCompile(`Category:\s*"([a-z0-9_]+)"`)
	categoryConstant = regexp.MustCompile(
		`(?m)^\s*(?:const\s+)?[A-Za-z_]*[Cc]ategory[A-Za-z_]*\s*(?:string\s*)?=\s*"([a-z0-9_]+)"`)
	ruleCats = regexp.MustCompile(`cats\(([^)]*)\)`)
	quoted   = regexp.MustCompile(`"([a-z0-9_]+)"`)
	// findingConstant is a constant naming a finding's category, such as
	// the users_email_case migration's.
	findingConstant = regexp.MustCompile(
		`(?m)^\s*(?:const\s+)?[A-Za-z_]*Finding\s*(?:string\s*)?=\s*"([a-z0-9_]+)"`)
	// sqlLiteral is a category written as the first VALUES literal of an
	// INSERT INTO sage.findings inside one SQL string.
	sqlLiteral = regexp.MustCompile(
		`(?s)INSERT INTO sage\.findings[^;` + "`" + `]*?VALUES\s*\(\s*'([a-z0-9_]+)'`)
)

// emittedCategories scans the non-test Go files under internal/ and cmd/
// for Category: literals, *Category* and *Finding constants, rule cats(...)
// lists and categories written as SQL literals into sage.findings.
// internal/testsupport is skipped: its fixtures build test data (such as
// backtestfx's snapshot rows, whose Category is a snapshot category) and
// never produce a finding.
func emittedCategories(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	support := filepath.Join("..", "testsupport")
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && path == support {
				return filepath.SkipDir
			}
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range categoryLiteral.FindAllSubmatch(src, -1) {
				found[string(m[1])] = path
			}
			for _, re := range []*regexp.Regexp{categoryConstant, findingConstant,
				sqlLiteral} {
				for _, m := range re.FindAllSubmatch(src, -1) {
					found[string(m[1])] = path
				}
			}
			for _, m := range ruleCats.FindAllSubmatch(src, -1) {
				for _, q := range quoted.FindAllSubmatch(m[1], -1) {
					found[string(q[1])] = path
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}
	return found
}

func TestEveryEmittedCategoryIsRegistered(t *testing.T) {
	found := emittedCategories(t)
	for _, must := range []string{"missing_index", "sage_self_cost", "sage_self_budget",
		"sage_footprint", "temp_spill", "clone_schemas", "app_managed_index",
		"managed_storage_runway", "audit_chain_broken", "forecast_wal_runway",
		"sre_action", "sre_maintenance", "auth_email_case_duplicates"} {
		if _, ok := found[must]; !ok {
			t.Fatalf("the scan missed %q: it is not reading the producers", must)
		}
	}
	for name, path := range found {
		if _, ok := retired[name]; ok {
			continue
		}
		c, ok := Lookup(name)
		if !ok || c.Prefix {
			t.Errorf("%s emits %q, which has no registry entry", path, name)
		}
	}
	for name, path := range retired {
		if _, ok := found[name]; !ok {
			t.Errorf("retired %q is gone from %s: drop it from the list", name, path)
		}
	}
}
