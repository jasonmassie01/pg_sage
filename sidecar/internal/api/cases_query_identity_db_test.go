package api

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
)

// A query finding's case identity and its parameterization script name
// the query by object identifier (cases-10). No producer writes
// detail.normalized_query, so a value planted there is ignored by the Go
// identity, the script, and the SQL mirror the ?case= lookup uses.

const plantedQueryKey = "normalized_query"

func plantedQueryFinding() cases.SourceFinding {
	return cases.SourceFinding{
		ID: "param-1", DatabaseName: "prod", Category: "query_parameterization",
		Severity: cases.SeverityWarning, ObjectType: "query", ObjectIdentifier: "queryid:88",
		Title: "Literal-heavy query should be parameterized",
		Detail: map[string]any{
			plantedQueryKey: "select * from planted where id = ?",
			"literal_count": float64(1200),
		},
	}
}

func TestQueryFindingIdentityIgnoresPlantedNormalizedQuery(t *testing.T) {
	got := cases.ProjectFinding(plantedQueryFinding(), executor.ClassifySQL)
	if got.ID != "finding:prod:query_parameterization:query:queryid:88" {
		t.Fatalf("case identity = %q, want the object identifier", got.ID)
	}
	if len(got.ActionCandidates) != 1 || got.ActionCandidates[0].ScriptOutput == nil {
		t.Fatalf("candidates = %+v, want one with a script", got.ActionCandidates)
	}
	script := got.ActionCandidates[0].ScriptOutput.MigrationSQL
	if !strings.Contains(script, "-- queryid:88\n") || strings.Contains(script, "planted") {
		t.Fatalf("migration script = %q, want the object identifier only", script)
	}
}

// The ?case= lookup's SQL mirror finds the planted finding by the same
// identity the Go projection gives it (real PG).
func TestFocusedQueryFindingIgnoresPlantedNormalizedQuery(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status)
		VALUES ('query_parameterization', 'warning', 'query', 'queryid:88', 'literal heavy',
		        jsonb_build_object($1::text, 'select * from planted where id = ?'),
		        'parameterize', 'open')`, plantedQueryKey); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	mgr := dbCaseManager(t, ctx, pool)
	identity := "finding:prod:query_parameterization:query:queryid:88"
	found, ok, err := lookupCase(ctx, mgr, namedPool{name: "prod", pool: pool}, identity)
	if err != nil || !ok || found.ID != identity {
		t.Fatalf("lookupCase = %q, %v, %v; want the finding by its object identifier",
			found.ID, ok, err)
	}
	planted := "finding:prod:query_parameterization:query:select * from planted where id = ?"
	if _, ok, err := lookupCase(ctx, mgr, namedPool{name: "prod", pool: pool},
		planted); err != nil || ok {
		t.Fatalf("lookup by the planted key = %v, %v; want not found", ok, err)
	}
}
