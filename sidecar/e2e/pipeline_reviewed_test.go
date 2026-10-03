//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
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
	// A reviewed optimizer index is one HypoPG verified; an unverified one
	// needs approval and is covered by TestPipelineUnverifiedIndexNeedsApproval.
	f.Detail["what_if_verdict"] = optimizer.WhatIfVerified
	driveFinding(t, pool, an, ex, f)
	// Without load evidence the autonomous build is withheld (D6): recorded
	// once in sage.admission_withheld, never as a failed action row.
	var withheld int
	err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.admission_withheld
		WHERE finding_key = $1 AND mode = 'unavailable'`,
		fmt.Sprintf("finding:%d", pipelineFindingID(t, pool, f))).Scan(&withheld)
	if err != nil || withheld != 1 {
		t.Fatalf("CREATE not withheld without load evidence: withheld=%d err=%v", withheld, err)
	}
	var built int
	err = pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.action_log
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
	quoted := pgx.Identifier(parts).Sanitize()
	var queryID int64
	// A pg_stat_statements_reset() elsewhere on the server can erase the
	// workload before its identity is read: repeat then.
	pgssepoch.Attempt(t, t.Context(), pool, 3, func() []string {
		mustExec(t, pool, "SELECT count(*) FROM "+quoted)
		err := pool.QueryRow(t.Context(), `SELECT queryid FROM pg_stat_statements
			WHERE query LIKE $1
			  AND dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
			ORDER BY calls DESC LIMIT 1`, "%FROM "+quoted+"%").Scan(&queryID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && queryID == 0) {
			return []string{fmt.Sprintf("read real workload identity: %d %v", queryID, err)}
		}
		if err != nil {
			t.Fatalf("read real workload identity: %v", err)
		}
		return nil
	})
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
		f.RecommendedSQL, f.RollbackSQL, &operatorID)
	if err != nil || id <= 0 {
		t.Fatalf("approved action: action=%d err=%v", id, err)
	}
}
