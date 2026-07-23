package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestWave4BackendSignalsAlwaysRequireApproval(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Moderate = true
	cfg.Trust.MaintenanceWindow = "* * * * *"
	now := time.Now()
	for _, actionType := range []string{"cancel_backend", "terminate_backend"} {
		contract, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("missing contract for %s", actionType)
		}
		decision := EvaluateActionPolicy(contract, ActionPolicyContext{
			Config:        cfg,
			ExecutionMode: "auto",
			Now:           now,
			RampStart:     now.Add(-90 * 24 * time.Hour),
		})
		if decision.Decision != PolicyDecisionQueueApproval || !decision.RequiresApproval {
			t.Fatalf("%s decision=%+v, want approval queue", actionType, decision)
		}
	}
}

func TestWave4BackendSignalSQLRequiresOnePositiveLiteralPID(t *testing.T) {
	valid := []string{
		"SELECT pg_cancel_backend(42)",
		"SELECT pg_terminate_backend(42);",
	}
	for _, sql := range valid {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("valid backend signal %q rejected: %v", sql, err)
		}
	}
	invalid := []string{
		"SELECT pg_cancel_backend(0)",
		"SELECT pg_cancel_backend(-1)",
		"SELECT pg_cancel_backend(41 + 1)",
		"SELECT pg_cancel_backend((SELECT pid FROM pg_stat_activity LIMIT 1))",
		"SELECT pg_terminate_backend(42) FROM pg_stat_activity",
	}
	for _, sql := range invalid {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Errorf("non-literal or ambiguous backend signal %q was accepted", sql)
		}
	}
}

func TestWave4RunawayDisabledMeansNoTrackingOrFindings(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false
	tracker := NewRunawayTracker(cfg, 1, nopLog)
	query := makeActiveQuery(222, time.Now().Add(-10*time.Minute), "application")
	for i := 0; i < 10; i++ {
		if findings := tracker.Evaluate([]ActiveQuery{query}, nil); len(findings) != 0 {
			t.Fatalf("disabled tracker produced findings on cycle %d: %v", i+1, findings)
		}
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.tracked) != 0 {
		t.Fatalf("disabled tracker retained %d queries, want 0", len(tracker.tracked))
	}
}

func TestWave4ApprovedBackendSignalRejectsStaleEvidence(t *testing.T) {
	pool, ctx := requireDB(t)
	_ = SetEmergencyStop(ctx, pool, false)

	victim, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire victim connection: %v", err)
	}
	defer victim.Release()
	var pid int
	if err := victim.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("victim pid: %v", err)
	}

	sql := fmt.Sprintf("SELECT pg_cancel_backend(%d)", pid)
	detail := fmt.Sprintf(
		`{"pid":%d,"query_id":998877,"query_start":"2000-01-01T00:00:00Z",`+
			`"query":"SELECT pg_sleep(60)"}`,
		pid,
	)
	var findingID int
	err = pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, recommended_sql)
		 VALUES ('wave4_stale_runaway', 'critical', 'process', $1,
		         'stale runaway evidence', $2::jsonb, 'review', $3)
		 RETURNING id`,
		fmt.Sprintf("pid:%d", pid), detail, sql,
	).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx,
			"DELETE FROM sage.action_log WHERE finding_id = $1", findingID)
		_, _ = pool.Exec(cleanupCtx,
			"DELETE FROM sage.findings WHERE id = $1", findingID)
	})

	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	exec := &Executor{
		pool:          pool,
		cfg:           cfg,
		recentActions: make(map[string]time.Time),
		logFn:         nopLog,
		execMode:      "approval",
	}
	approver := 7
	_, err = exec.ExecuteManual(ctx, findingID, sql, "", &approver)
	if err == nil || (!strings.Contains(err.Error(), "stale") &&
		!strings.Contains(err.Error(), "evidence")) {
		t.Fatalf("stale backend evidence was not rejected: %v", err)
	}
}

func TestWave4SessionSettingFailureCannotPoisonPool(t *testing.T) {
	_, _ = requireDB(t)
	poolCfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	poolCfg.MaxConns = 1
	poolCfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping isolated pool: %v", err)
	}

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire baseline connection: %v", err)
	}
	if _, err := conn.Exec(t.Context(), "SET statement_timeout = 0"); err != nil {
		conn.Release()
		t.Fatalf("reset baseline timeout: %v", err)
	}
	conn.Release()

	err = ExecConcurrently(
		t.Context(), pool, "ANALYZE public.missing_timeout_fixture", 1234*time.Millisecond,
		WithLockTimeout(-1),
	)
	if err == nil {
		t.Fatal("negative lock timeout unexpectedly succeeded")
	}

	conn, err = pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("reacquire after setting failure: %v", err)
	}
	defer conn.Release()
	var timeout string
	if err := conn.QueryRow(t.Context(), "SHOW statement_timeout").Scan(&timeout); err != nil {
		t.Fatalf("show statement_timeout: %v", err)
	}
	if timeout != "0" && timeout != "0ms" && timeout != "0s" {
		t.Fatalf("pooled session retained statement_timeout %q", timeout)
	}
}
