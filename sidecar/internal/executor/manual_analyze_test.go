package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestExecuteManualAnalyzeUsesDedicatedAnalyzePath(t *testing.T) {
	pool, ctx := requireDB(t)
	_ = SetEmergencyStop(ctx, pool, false)

	const tableName = "sage.test_manual_analyze_path"
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS sage.test_manual_analyze_path`)
	_, err := pool.Exec(ctx,
		`CREATE TABLE sage.test_manual_analyze_path (id int)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO sage.test_manual_analyze_path (id) VALUES (1), (2)`)
	if err != nil {
		t.Fatalf("insert rows: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_log WHERE sql_executed = $1",
			"ANALYZE sage.test_manual_analyze_path")
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.findings WHERE object_identifier = $1",
			tableName)
		_, _ = pool.Exec(cctx,
			`DROP TABLE IF EXISTS sage.test_manual_analyze_path`)
	})

	recentAnalyzesMu.Lock()
	delete(recentAnalyzes, tableName)
	recentAnalyzesMu.Unlock()

	var findingID int
	err = pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, recommended_sql)
		 VALUES ('stale_statistics', 'warning', 'table', $1,
		         'manual analyze stale stats', '{}', 'run analyze',
		         'ANALYZE sage.test_manual_analyze_path')
		 RETURNING id`,
		tableName,
	).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	e := &Executor{
		pool: pool,
		cfg: &config.Config{
			Safety: config.SafetyConfig{
				DDLTimeoutSeconds: 10,
				LockTimeoutMs:     5000,
			},
			Tuner: config.TunerConfig{
				AnalyzeCooldownMinutes: 30,
				AnalyzeTimeoutMs:       10000,
			},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		shutdownCh:    make(chan struct{}),
	}

	actionID, err := e.ExecuteManual(
		ctx, findingID, "ANALYZE sage.test_manual_analyze_path", "", nil)
	if err != nil {
		t.Fatalf("ExecuteManual analyze: %v", err)
	}
	if actionID <= 0 {
		t.Fatalf("actionID=%d, want > 0", actionID)
	}
	if !checkAnalyzeCooldown(tableName, 30) {
		t.Fatalf("manual analyze did not mark analyze cooldown")
	}
}
