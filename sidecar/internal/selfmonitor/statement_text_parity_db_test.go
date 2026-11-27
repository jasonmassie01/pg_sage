package selfmonitor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-11: the Go predicate and the SQL predicate agree on every
// case, on a real server (ILIKE wildcards, ~* and NULL as PostgreSQL
// evaluates them).
func TestStatementExclusion_GoMatchesSQL(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	sql := "SELECT " + StatementExclusionSQL("$1::text")
	for _, tc := range statementTextCases {
		var application bool
		if err := conn.QueryRow(ctx, sql, tc.text).Scan(&application); err != nil {
			t.Fatalf("evaluate the SQL predicate on %v: %v", tc.text, err)
		}
		text := ""
		if tc.text != nil {
			text = *tc.text
		}
		if application == tc.self {
			t.Errorf("SQL predicate on %q: application %v, want %v", text, application,
				!tc.self)
		}
		if IsStatementText(text) != !application {
			t.Errorf("Go and SQL disagree on %q: Go self %v, SQL application %v", text,
				IsStatementText(text), application)
		}
	}
}
