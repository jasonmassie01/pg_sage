package firstlook

import (
	"context"
	"testing"
)

// The first look's catalog reads are planned with costs in the millions
// (the statistics views' joins), so with jit on the server compiled 67
// functions for the index read: 635-986 ms cold in the nightly perf gate,
// over the 500 ms catalog budget, for a query that runs in tens of ms.
// Its transaction turns JIT off, also when the operator's lower timeout
// is kept, and leaves the session's own setting alone.
func TestOpenTurnsJITOffForTheTransaction(t *testing.T) {
	admin, ctx := livePool(t)
	for _, tc := range []struct {
		name, sessionTimeout string
	}{
		{"own budget", "0"},
		{"operator's lower timeout kept", "300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := sessionTimeoutPool(t, ctx, admin, tc.sessionTimeout)
			var before string
			if err := pool.QueryRow(ctx, "SELECT current_setting('jit')").Scan(&before); err != nil {
				t.Fatal(err)
			}
			p := &pass{pool: pool, opts: testOptions("app"), report: &Report{}}
			if err := p.open(ctx); err != nil {
				t.Fatalf("open: %v", err)
			}
			var jit string
			if err := p.tx.QueryRow(ctx, "SELECT current_setting('jit')").Scan(&jit); err != nil {
				t.Fatalf("read jit: %v", err)
			}
			p.close()
			if jit != "off" {
				t.Fatalf("jit inside the first look = %q, want off", jit)
			}
			var after string
			if err := pool.QueryRow(context.Background(),
				"SELECT current_setting('jit')").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("jit after the first look = %q: the session setting leaked", after)
			}
		})
	}
}
