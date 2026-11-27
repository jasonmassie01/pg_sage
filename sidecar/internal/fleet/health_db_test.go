package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Real Postgres: sage.health_history records the staleness-capped score,
// for both the single and the fleet-wide snapshot paths.
func TestRecordHealthSnapshot_StaleScoreOnPostgres(t *testing.T) {
	pool := isolatedStopDB(t)
	mgr := NewManager(&config.Config{
		Mode:      "fleet",
		Collector: config.CollectorConfig{IntervalSeconds: 60},
	})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:   "stale",
		Config: config.DatabaseConfig{Name: "stale"},
		Pool:   pool,
		Status: &InstanceStatus{
			Connected: true, FindingsWarning: 1,
			CollectorLastRun: time.Now().Add(-time.Hour),
		},
	})
	ctx := context.Background()
	mgr.RecordHealthSnapshot(ctx, "stale")
	mgr.RecordHealthSnapshots(ctx)

	rows, err := pool.Query(ctx,
		`SELECT health_score, findings_warning FROM sage.health_history
		 WHERE database_name = $1`, "stale")
	if err != nil {
		t.Fatalf("read health_history: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var score, warnings int
		if err := rows.Scan(&score, &warnings); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if score != 25 || warnings != 1 {
			t.Fatalf("row %d: score %d warnings %d, want 25 and 1", n, score, warnings)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d health_history rows, want 2 (single + fleet path)", n)
	}
}
