//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/executor"
)

// SQL-shape coverage must not grant fictitious host-load telemetry. These cases
// prove automatic withholding first, then the supported explicitly reviewed path.
func driveReviewedIndexFinding(
	t *testing.T, pool *pgxpool.Pool, an *analyzer.Analyzer,
	ex *executor.Executor, f analyzer.Finding,
) {
	t.Helper()
	if f.Detail == nil {
		f.Detail = make(map[string]any)
	}
	f.Detail["queryids"] = []int64{pipelineQueryID(t, pool, f.ObjectIdentifier)}
	driveFinding(t, pool, an, ex, f)
	act, ok := latestActionFor(t, pool, f.Category, f.ObjectIdentifier)
	if !ok || act.Outcome != "failed" || !strings.Contains(act.RollbackReason, "telemetry") {
		t.Fatalf("CREATE admitted without host-load evidence: %#v exists=%v", act, ok)
	}
	var built int
	err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.action_log
		WHERE finding_id=$1 AND outcome IN ('success','monitoring')`,
		pipelineFindingID(t, pool, f)).Scan(&built)
	if err != nil || built != 0 {
		t.Fatalf("unexpected automatic build: %d %v", built, err)
	}
	id, err := ex.ExecuteManual(t.Context(), pipelineFindingID(t, pool, f),
		f.RecommendedSQL, f.RollbackSQL, nil)
	if err != nil || id <= 0 {
		t.Fatalf("reviewed index shape failed: action=%d err=%v", id, err)
	}
}

func pipelineQueryID(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	parts := strings.Split(table, ".")
	if len(parts) != 2 {
		t.Fatal("SQL-shape fixture requires schema.table")
	}
	mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	mustExec(t, pool, "SELECT count(*) FROM "+pgx.Identifier(parts).Sanitize())
	var queryID int64
	err := pool.QueryRow(t.Context(), `SELECT queryid FROM pg_stat_statements
		WHERE query LIKE $1 AND dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
		ORDER BY calls DESC LIMIT 1`, "%FROM "+pgx.Identifier(parts).Sanitize()+"%").Scan(&queryID)
	if err != nil || queryID == 0 {
		t.Fatalf("read real workload identity: %d %v", queryID, err)
	}
	return queryID
}

func pipelineFindingID(t *testing.T, pool *pgxpool.Pool, f analyzer.Finding) int {
	t.Helper()
	var id int
	err := pool.QueryRow(t.Context(), `SELECT id FROM sage.findings
		WHERE category=$1 AND object_identifier=$2 ORDER BY id DESC LIMIT 1`,
		f.Category, f.ObjectIdentifier).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func approvePipelineCancel(
	t *testing.T, pool *pgxpool.Pool, ex *executor.Executor, f analyzer.Finding,
) {
	t.Helper()
	var operatorID int
	email := fmt.Sprintf("pipeline-cancel-%d@pg-sage.test", pipelineFindingID(t, pool, f))
	err := pool.QueryRow(t.Context(), `INSERT INTO sage.users(email,password,role)
		VALUES ($1,'test-only-unusable-hash','admin') RETURNING id`, email).Scan(&operatorID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM sage.users WHERE id=$1",
			operatorID); err != nil {
			t.Error(err)
		}
	})
	id, err := ex.ExecuteManual(t.Context(), pipelineFindingID(t, pool, f),
		f.RecommendedSQL, "", &operatorID)
	if err != nil || id <= 0 {
		t.Fatalf("approved cancel: action=%d err=%v", id, err)
	}
}
