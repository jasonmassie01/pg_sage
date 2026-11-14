package changefeed

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the stats_reset source reads pg_stat_statements_info in
// schema ext through a session pinned to pg_catalog, pg_temp.
func TestStatsResetExtensionOutsideSearchPath(t *testing.T) {
	_, pool := testdb.ExtSchemaDatabase(t, "changefeedext")
	got, err := scanStatsReset(context.Background(), pool)
	if err != nil {
		t.Fatalf("scanStatsReset: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, got); err != nil {
		t.Fatalf("scanStatsReset = %q, want an RFC 3339 time: %v", got, err)
	}
}
