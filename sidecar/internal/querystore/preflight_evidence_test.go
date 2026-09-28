package querystore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// No concurrent access: these fixtures test ordered sample semantics, not worker contention.
func TestPreflightEvidenceInteriorResetWindow(t *testing.T) {
	ctx := context.Background()
	p, err := pgxpool.New(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		samples []Sample
		wantOK  bool
	}{
		{"stable", []Sample{{Calls: 100, TotalExecMs: 1000}, {Calls: 200, TotalExecMs: 3000}}, true},
		{"reset_regrown", []Sample{{Calls: 100, TotalExecMs: 1000},
			{Calls: 1, TotalExecMs: 10}, {Calls: 200, TotalExecMs: 4000}}, false},
		{"no_calls", []Sample{{Calls: 100, TotalExecMs: 1000}, {Calls: 100, TotalExecMs: 1000}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = int64(8901234)
			if _, err := p.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", id); err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.samples {
				s.QueryID = id
				if err := Record(ctx, p, []Sample{s}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * time.Millisecond)
			}
			// Adapted setup: captured_at is assigned by the server clock, so
			// the window bounds come from the same clock. The fixture server
			// runs ~40ms ahead of this host, which excluded the last sample
			// from a client-clock window and made "stable" flaky.
			var now time.Time
			if err := p.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
				t.Fatal(err)
			}
			ms, ok, err := WindowedLatencyMsBetween(ctx, p, id, now.Add(-time.Minute), now)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("samples=%+v ms=%f ok=%v", tc.samples, ms, ok)
			if ok != tc.wantOK {
				t.Errorf("window validity=%v want %v", ok, tc.wantOK)
			}
			if tc.name == "stable" && ms != 20 {
				t.Errorf("stable latency=%f want20", ms)
			}
		})
	}
}
