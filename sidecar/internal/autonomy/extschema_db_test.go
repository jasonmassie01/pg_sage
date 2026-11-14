package autonomy

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the statement index and the bloat confirmation read
// pg_stat_statements and pgstattuple_approx in schema ext through a
// session pinned to pg_catalog, pg_temp.
func TestAutonomyReadersFindExtensionsOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "autonomyext")
	testdb.SeedStatement(t, admin, pool, "SELECT count(*) FROM pg_catalog.pg_type", 2)
	idx, err := loadStatementIndex(ctx, pool)
	if err != nil || idx.unavailable != nil || !idx.known {
		t.Fatalf("loadStatementIndex = %+v, %v; want a known read", idx, err)
	}
	fn, err := pgstattupleFunc(ctx, pool)
	if err != nil || fn != `"ext"."pgstattuple_approx"` {
		t.Fatalf("pgstattupleFunc = %q, %v", fn, err)
	}
	var ratio *float64
	const pgClassOID = int64(1259)
	if err := pool.QueryRow(ctx, pgstattupleConfirmSQL(fn), pgClassOID).Scan(&ratio); err != nil ||
		ratio == nil {
		t.Fatalf("bloat confirmation = %v, %v; want a measured ratio", ratio, err)
	}
}

// Nil/empty: without pgstattuple the function is absent (42883), the
// error a bare pgstattuple_approx raised.
func TestPgstattupleFuncAbsent(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "autonomynopgst")
	if _, err := admin.Exec(ctx, "DROP EXTENSION pgstattuple"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if fn, err := pgstattupleFunc(ctx, pool); !errors.Is(err, extschema.ErrExtensionAbsent) {
		t.Fatalf("pgstattupleFunc = %q, %v; want ErrExtensionAbsent", fn, err)
	}
}
