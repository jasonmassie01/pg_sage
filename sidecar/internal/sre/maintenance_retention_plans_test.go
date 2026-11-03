package sre

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// The initial hold prefilter must remain a correlated lookup by the full
// investigation identity, even with thousands of maintenance proposals in the
// same scope. Otherwise retention hashes/scans the entire ledger each batch.
func TestRetention_MaintenanceCandidateGenericPlanUsesScopeIndex(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	scope := testScope(t, ctx, st)
	if _, err := pool.Exec(ctx, `WITH investigations AS (
		INSERT INTO sage.sre_investigations (deployment_id,database_id,id,
		    source_case_id,trigger_kind,trigger_fingerprint,state,created_at,updated_at,expires_at)
		SELECT $1,$2,gen_random_uuid(),'maintenance-plan:'||g,'operator',
		    sha256(convert_to(g::text,'UTF8')),'concluded',
		    now()-interval '401 days',now()-interval '400 days',now()+interval '1 day'
		FROM generate_series(1,6000) g RETURNING deployment_id,database_id,id
	)
	INSERT INTO sage.sre_maintenance_proposals
	    (id,deployment_id,database_id,investigation_id,state,proposal,created_at,updated_at)
	SELECT gen_random_uuid(),deployment_id,database_id,id,'uncertain','{}'::jsonb,
	    now()-interval '401 days',now()-interval '400 days' FROM investigations`,
		string(scope.DeploymentID), string(scope.DatabaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE sage.sre_investigations`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE sage.sre_maintenance_proposals`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	result, err := conn.Conn().PgConn().Exec(ctx,
		"EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) "+agedIDsSQL(keepHeldBudget)).ReadAll()
	if err != nil || len(result) != 1 || len(result[0].Rows) != 1 {
		t.Fatalf("explain maintenance candidate query: %v", err)
	}
	raw := result[0].Rows[0][0]
	scans, err := perfgate.SeqScans(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, scan := range scans {
		if scan.Relation == "sre_maintenance_proposals" {
			t.Fatalf("maintenance candidate predicate scans its ledger: %s", raw)
		}
	}
	var index string
	if err := conn.QueryRow(ctx, `SELECT indexname FROM pg_indexes
		WHERE schemaname='sage' AND tablename='sre_maintenance_proposals'
		  AND indexdef LIKE '%(deployment_id, database_id, investigation_id)%'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"Index Name": "`+index+`"`) {
		t.Fatalf("maintenance candidate predicate did not use full-scope index %s: %s", index, raw)
	}
	t.Logf("6000 same-scope maintenance holds use correlated index %s", index)
}
