package pgaudit

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// Boundary (transition): an untagged pg_sage DDL record whose action is
// recorded 4 minutes later is stored, linked by its SQL.
func TestIngestHeldUntaggedRecordMatchedLater(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_hold")
	ctx := context.Background()
	start := time.Now()
	now := start
	c := NewCorrelator(pool, "orders", nil)
	c.now = func() time.Time { return now }
	rec := entry("pg_sage", sessionUser(t, pool), cicRecord)
	if st, err := c.Ingest(ctx, []logwatch.LogEntry{rec}); err != nil || st.Held != 1 {
		t.Fatalf("first ingest = %+v, %v; want held", st, err)
	}
	now = start.Add(4 * time.Minute)
	action := seedAction(t, pool, seedDecision(t, pool), rec.Timestamp.Add(4*time.Minute))
	if st, err := c.Ingest(ctx, nil); err != nil || st.Stored != 1 {
		t.Fatalf("ingest 4 minutes on = %+v, %v; want stored", st, err)
	}
	if count(t, pool, "correlated_by = 'statement' AND action_id = "+itoa(action)) != 1 {
		t.Fatalf("held record not linked to action %d", action)
	}
}
