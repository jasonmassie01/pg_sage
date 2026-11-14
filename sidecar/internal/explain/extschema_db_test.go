package explain

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: query_id resolution reads pg_stat_statements in schema
// ext through a session pinned to pg_catalog, pg_temp; without the
// extension it is the same invalid request as before.
func TestResolveQueryIDExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "explainext")
	const probe = "SELECT count(*) FROM pg_catalog.pg_namespace"
	id := testdb.SeedStatement(t, admin, pool, probe, 2)
	text, err := ResolveQueryID(ctx, pool, id)
	if err != nil || text != probe {
		t.Fatalf("ResolveQueryID = %q, %v; want %q", text, err, probe)
	}
	if _, err := admin.Exec(ctx, "DROP EXTENSION pg_stat_statements"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, err = ResolveQueryID(ctx, pool, id)
	if !errors.Is(err, ErrStatementsUnavailable) || !errors.Is(err, ErrExplainInvalidRequest) {
		t.Fatalf("without the extension err = %v, want ErrStatementsUnavailable", err)
	}
}
