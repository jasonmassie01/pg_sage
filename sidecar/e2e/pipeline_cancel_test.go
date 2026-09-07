//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

func startPipelineSleeper(t *testing.T) (int, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	victim, err := pgx.Connect(ctx, pipelineURL(t))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var pid int
	if err := victim.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		cancel()
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := victim.Exec(ctx, "SELECT pg_sleep(30)")
		errCh <- err
	}()
	t.Cleanup(func() { cancel(); _ = victim.Close(context.Background()) })
	return pid, errCh
}

func pipelineCancelFinding(t *testing.T, pool *pgxpool.Pool, pid int) analyzer.Finding {
	t.Helper()
	var start time.Time
	var query, app string
	var queryID int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(t.Context(), `SELECT query_start, COALESCE(query_id,0),
			LEFT(query,200), application_name FROM pg_stat_activity
			WHERE pid=$1 AND state='active' AND query LIKE '%pg_sleep%'`, pid).
			Scan(&start, &queryID, &query, &app)
		if err == nil {
			break
		}
		if err != pgx.ErrNoRows {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if start.IsZero() {
		t.Fatal("victim did not become active")
	}
	return analyzer.Finding{
		Category: "runaway_query", Severity: "warning", ObjectType: "backend",
		ObjectIdentifier: fmt.Sprintf("pid:%d", pid), Title: "runaway query",
		RecommendedSQL: fmt.Sprintf("SELECT pg_cancel_backend(%d);", pid), ActionRisk: "moderate",
		Detail: map[string]any{"pid": pid, "query_id": queryID, "query_start": start,
			"query": query, "app_name": app},
	}
}
