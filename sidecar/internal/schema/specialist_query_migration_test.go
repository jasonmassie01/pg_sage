package schema

import (
	"context"
	"strings"
	"testing"
)

// Specialist contract revision 1.1.0: sage.specialist_requests.query_scope
// keeps the statement a caller scoped its investigation to (NULL for v1
// requests). Additive and idempotent.

func TestSpecialistQueryScopeMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var typ string
	if err := pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'specialist_requests'
		  AND column_name = 'query_scope'`).Scan(&typ); err != nil || typ != "jsonb" {
		t.Fatalf("query_scope column: %q %v", typ, err)
	}
	if !strings.Contains(ddlSpecialistQueryScope, "IF NOT EXISTS") {
		t.Fatal("the migration is not idempotent")
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.specialist_requests WHERE token_id LIKE 'mig-q:%'")
	}
	clean()
	t.Cleanup(clean)
	insert := `INSERT INTO sage.specialist_requests (kind, token_id, actor, transport,
		database_name, query_scope) VALUES ('open', $1, 'a', 'http', 'orders', $2::jsonb)`
	if _, err := pool.Exec(ctx, insert, "mig-q:1",
		`{"query_id":"42","applied":"evidence"}`); err != nil {
		t.Fatalf("valid scope: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "mig-q:2", nil); err != nil {
		t.Fatalf("a v1 request without a scope: %v", err)
	}
	for _, bad := range []string{`"42"`, `[42]`, `{"query_id":"` +
		strings.Repeat("9", 3000) + `"}`} {
		if _, err := pool.Exec(ctx, insert, "mig-q:3", bad); err == nil {
			t.Errorf("scope %.40s accepted", bad)
		}
	}
}
