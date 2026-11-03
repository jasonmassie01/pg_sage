package sre

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// A newer or noncanonical schema can contain a state this binary does not
// understand. It must retain the ledger/evidence, rather than treat that state
// as a finalized outcome. The supported Bootstrap check itself stays canonical.
func TestRetention_UnknownMaintenanceStateFailsClosed(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "maintenance_unknown"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st, err := NewPostgresStore(pool, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope(t, ctx, st)
	id := agedConcluded(t, ctx, st, pool, scope, "future maintenance state", 400)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.sre_maintenance_proposals
		DROP CONSTRAINT sre_maintenance_proposals_state_check;
		ALTER TABLE sage.sre_maintenance_proposals
		ADD CONSTRAINT sre_maintenance_proposals_state_check
		CHECK (state IN ('proposed','requested','executing','executed','uncertain',
		    'failed','refused','rolling_back','rolled_back','rollback_failed','future_state'))`); err != nil {
		t.Fatal(err)
	}
	insertRetentionMaintenance(t, ctx, pool, scope, id, "future_state", 400)
	res, err := st.Purge(ctx, scope, testPolicy)
	if err != nil || res != (PurgeResult{}) {
		t.Fatalf("unknown maintenance outcome was treated as finalized: %+v %v", res, err)
	}
	if n := countRows(t, ctx, pool, "sre_evidence", id); n != 2 {
		t.Fatalf("unknown maintenance outcome lost evidence: %d rows", n)
	}
	if err := st.VerifyEvents(ctx, scope, id); err != nil {
		t.Fatal(err)
	}
}
