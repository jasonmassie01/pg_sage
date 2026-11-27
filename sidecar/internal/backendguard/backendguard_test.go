package backendguard

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// No concurrent-access, error or DB tests: the package holds SQL constants
// only. Their behaviour on real PostgreSQL is covered by the executor and
// cases guarded-signal DB tests that embed them.

var notILike = regexp.MustCompile(`a\.application_name NOT ILIKE '([^']+)'`)

func TestProtectedPredicateExcludesNonClientAndTools(t *testing.T) {
	for _, want := range []string{
		"AND a.datname = current_database()",
		"AND a.backend_type = 'client backend'",
	} {
		if !strings.Contains(ProtectedPredicate, want) {
			t.Fatalf("ProtectedPredicate lacks %q:\n%s", want, ProtectedPredicate)
		}
	}
	got := patterns(ProtectedPredicate)
	want := []string{"%pg_sage%", "%pg_dump%", "%pg_basebackup%", "%pg_restore%"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded applications = %v, want %v", got, want)
	}
}

func TestTerminatePredicateRefusesSuperusersAndAutovacuum(t *testing.T) {
	if !strings.Contains(TerminatePredicate, "AND NOT r.rolsuper") {
		t.Fatalf("TerminatePredicate lacks the superuser guard:\n%s", TerminatePredicate)
	}
	if got := patterns(TerminatePredicate); !reflect.DeepEqual(got, []string{"%autovacuum%"}) {
		t.Fatalf("terminate exclusions = %v, want [%%autovacuum%%]", got)
	}
	if !strings.Contains(TerminateJoin, "JOIN pg_roles AS r ON r.rolname = a.usename") {
		t.Fatalf("TerminateJoin = %q", TerminateJoin)
	}
}

// Every predicate line is an AND clause, so the constants append to any
// WHERE without changing its meaning.
func TestPredicatesAreAndClauses(t *testing.T) {
	for name, predicate := range map[string]string{
		"protected": ProtectedPredicate, "terminate": TerminatePredicate,
	} {
		for _, line := range strings.Split(strings.TrimSpace(predicate), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "AND ") {
				t.Fatalf("%s line %q is not an AND clause", name, line)
			}
		}
	}
}

func patterns(sql string) []string {
	var out []string
	for _, m := range notILike.FindAllStringSubmatch(sql, -1) {
		out = append(out, m[1])
	}
	return out
}
