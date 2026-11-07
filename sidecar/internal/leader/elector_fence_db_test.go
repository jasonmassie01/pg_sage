package leader

import (
	"context"
	"testing"
	"time"
)

// On PostgreSQL, the token is exactly the live lease row: same scope,
// holder and epoch, in every scope form the sidecar uses.
func TestElector_FenceTokenMatchesTheLeaseRow(t *testing.T) {
	ctx := context.Background()
	pool := dbPool(t)
	base := uniqueScope(t)
	for _, scope := range []string{base + ":meta", base + ":fleet:0123456789abcdef",
		base + ":standalone:app"} {
		e := NewElector(NewPostgresStore(pool), scope, "A", time.Minute)
		if err := e.Tick(ctx); err != nil {
			t.Fatalf("%s: tick: %v", scope, err)
		}
		tok, ok := e.Fence()
		if !ok || tok.Scope != scope || tok.Holder != "A" || tok.Epoch != 1 {
			t.Fatalf("%s: token = %+v,%v", scope, tok, ok)
		}
		var rows int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.fleet_leader_lease
			WHERE scope = $1 AND holder = $2 AND epoch = $3 AND expires_at > now()`,
			tok.Scope, tok.Holder, tok.Epoch).Scan(&rows)
		if err != nil || rows != 1 {
			t.Fatalf("%s: live lease rows for token = %d, err %v", scope, rows, err)
		}
		if err := e.Release(ctx); err != nil {
			t.Fatalf("%s: release: %v", scope, err)
		}
	}
}
