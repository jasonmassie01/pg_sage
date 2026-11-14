package broker

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: pg_stat_statements in schema ext, read on a session
// pinned to pg_catalog, pg_temp (no bare name resolves there).
func TestAttributionReadersFindExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "brokerext")
	const probe = "SELECT count(*) FROM pg_catalog.pg_class"
	testdb.SeedStatement(t, admin, pool, probe, 3)
	if info := readPSSInfo(ctx, pool); info == nil {
		t.Fatal("readPSSInfo = nil, want the info view read from ext")
	}
	var role string
	if err := admin.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	stmts, _, err := readStatements(ctx, pool, []string{role})
	if err != nil {
		t.Fatalf("readStatements: %v", err)
	}
	found := false
	for _, s := range stmts {
		found = found || (s.Query == probe && s.Calls >= 3)
	}
	if !found {
		t.Fatalf("readStatements = %+v, want %q with 3 calls", stmts, probe)
	}
}

// Nil/empty: without the extension the reader reports the view missing,
// as the bare name's undefined_table did.
func TestReadStatementsWithoutExtension(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "brokernoext")
	if _, err := admin.Exec(ctx, "DROP EXTENSION pg_stat_statements"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, _, err := readStatements(ctx, pool, []string{"postgres"}); err != errNoPSSView {
		t.Fatalf("readStatements err = %v, want errNoPSSView", err)
	}
	if info := readPSSInfo(ctx, pool); info != nil {
		t.Fatalf("readPSSInfo = %+v, want nil without the extension", info)
	}
}
