package testdb

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BurnXIDs consumes n XIDs on pool's server, one committed transaction
// each (synchronous_commit off, so a large n stays quick). Tests use it
// to age tables toward their freeze maximum.
func BurnXIDs(t testing.TB, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection to burn %d XIDs: %v", n, err)
	}
	defer conn.Release()
	sql := fmt.Sprintf(`DO $$ BEGIN
		PERFORM set_config('synchronous_commit', 'off', false);
		FOR i IN 1..%d LOOP PERFORM pg_catalog.pg_current_xact_id(); COMMIT; END LOOP;
		END $$`, n)
	if _, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("burn %d XIDs: %v", n, err)
	}
}
