package autonomy

import (
	"context"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestQualifiedNamesFindsUnquotedAndQuotedReferences(t *testing.T) {
	names := qualifiedNames(`SELECT * FROM Public.Orders o JOIN "Tenant_1"."Items" i
		ON o.id = i.order_id JOIN "we""ird".t3 w ON true`)
	for _, want := range []string{"public.orders", "tenant_1.items", `we"ird.t3`} {
		if !containsName(names, want) {
			t.Errorf("qualifiedNames missed %q in %v", want, names)
		}
	}
	chain := qualifiedNames("SELECT 1 FROM appdb.billing.invoices")
	if !containsName(chain, "billing.invoices") || !containsName(chain, "appdb.billing") {
		t.Errorf("three-part name pairs = %v", chain)
	}
	if got := qualifiedNames("SELECT now()"); len(got) != 0 {
		t.Errorf("unqualified statement yielded names %v", got)
	}
	if got := qualifiedNames(""); len(got) != 0 {
		t.Errorf("empty statement yielded names %v", got)
	}
	// A prefix is not a reference: public.orders must not match public.orders_archive.
	if containsName(qualifiedNames("SELECT * FROM public.orders_archive"), "public.orders") {
		t.Error("public.orders_archive was treated as a reference to public.orders")
	}
}

func TestIndexStatementsKeepsCallOrderAndCaps(t *testing.T) {
	rows := make([]statementRow, 0, 30)
	for i := 0; i < 30; i++ {
		rows = append(rows, statementRow{QueryID: int64(100 + i),
			Query: fmt.Sprintf("SELECT * FROM public.orders WHERE id = %d", i)})
	}
	rows = append(rows, statementRow{QueryID: 7,
		Query: "SELECT * FROM public.orders o, public.orders p"})
	index := indexStatements(rows)
	ids := index.queryIDs("public.orders")
	if len(ids) != maxQueryIDsPerTarget || ids[0] != 100 || ids[19] != 119 {
		t.Fatalf("query ids = %v, want the first %d by call order", ids, maxQueryIDsPerTarget)
	}
	if index.queryIDs("PUBLIC.ORDERS") == nil {
		t.Fatal("target lookup is case-sensitive; references are matched case-insensitively")
	}
	if !index.mentionsSchema("public") || index.mentionsSchema("tenant_000001") {
		t.Fatal("schema mentions are wrong")
	}
	if got := indexStatements(nil).queryIDs("public.orders"); got != nil {
		t.Fatalf("empty index query ids = %v", got)
	}
}

// Against PostgreSQL: pg_stat_statements text is read once per cycle; a
// missing extension is "no statement evidence", not an error.
func TestLoadStatementIndexToleratesMissingExtension(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	var installed bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname='pg_stat_statements')`).Scan(&installed); err != nil {
		t.Fatalf("read extensions: %v", err)
	}
	index, err := loadStatementIndex(ctx, pool)
	if err != nil {
		t.Fatalf("loadStatementIndex: %v", err)
	}
	if !installed && (!index.known || len(index.targets) != 0) {
		t.Fatalf("missing extension index = %+v, want known and empty", index)
	}
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements unavailable on this server: %v", err)
	}
	execAll(t, pool, "CREATE TABLE IF NOT EXISTS public.stmt_probe (id bigint)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.stmt_probe")
	})
	// A concurrent pg_stat_statements_reset() by another package can erase
	// the probe before the index is read: repeat then.
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		execAll(t, pool, "SELECT count(*) FROM public.stmt_probe")
		index, err := loadStatementIndex(ctx, pool)
		if err != nil {
			t.Fatalf("loadStatementIndex with extension: %v", err)
		}
		if len(index.queryIDs("public.stmt_probe")) == 0 || !index.mentionsSchema("public") {
			return []string{fmt.Sprintf("statement index misses public.stmt_probe: %d targets",
				len(index.targets))}
		}
		return nil
	})
}
