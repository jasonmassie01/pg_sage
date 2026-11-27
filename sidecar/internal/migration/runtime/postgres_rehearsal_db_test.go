//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// Integration (migration-06 part A): the runner measures every expand
// step with its lock, and the recorder stores contract_not_before as the
// clock plus the plan's delay.

func TestPostgresRehearsalRunnerMeasuresEveryStep(t *testing.T) {
	parsetree.Require(t)
	pool := migrationRuntimePool(t)
	ctx := context.Background()
	const table = "public.mig06_rehearsal_users"
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table+"; CREATE TABLE "+table+
		" (id bigint PRIMARY KEY, email text); INSERT INTO "+table+
		" SELECT g, 'u' || g FROM generate_series(1, 100) g"); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	planned, err := planpkg.NewPlanner().Plan(ctx, planpkg.Request{
		SQL:   "ALTER TABLE " + table + " ALTER COLUMN email SET NOT NULL",
		Table: planpkg.TableFacts{Schema: "public", Name: "mig06_rehearsal_users"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	runner := NewPostgresRehearsalRunner(time.Second, 10*time.Second).
		WithProduction(func(context.Context) (ServerIdentity, error) {
			return ServerIdentity{Host: "production.invalid", Port: 5432,
				StartedAt: time.Unix(1, 0), ServerPort: 5432}, nil
		})
	measurement, err := runner.Run(ctx, testdb.SkipUnlessLive(t), planned)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(measurement.Steps) != 2 {
		t.Fatalf("steps = %#v, want the two expand steps", measurement.Steps)
	}
	var total, longest time.Duration
	for i, step := range measurement.Steps {
		if step.Kind != planned.ExpandSteps[i].Kind || step.Duration <= 0 || step.Concurrent {
			t.Fatalf("step %d = %#v", i, step)
		}
		total += step.Duration
		longest = max(longest, step.Duration)
	}
	if measurement.Steps[0].LockLevel != "ACCESS EXCLUSIVE" ||
		measurement.Steps[1].LockLevel != "SHARE UPDATE EXCLUSIVE" {
		t.Fatalf("locks = %q, %q", measurement.Steps[0].LockLevel,
			measurement.Steps[1].LockLevel)
	}
	if measurement.MaxStepDuration != longest || measurement.BackfillDuration != total {
		t.Fatalf("max=%s backfill=%s, want %s and %s", measurement.MaxStepDuration,
			measurement.BackfillDuration, longest, total)
	}
}

// State transition: contract_not_before = the recorder's clock + 1h.
func TestPostgresRecorderStoresNowPlusContractDelay(t *testing.T) {
	pool := migrationRuntimePool(t)
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	recorder := NewPostgresRecorder(pool)
	recorder.now = func() time.Time { return now }
	record := Record{Request: request(), Verdict: VerdictExpanded,
		EvidenceID: "ev_mig06_delay", Reason: ReasonContractPending,
		ContractDelay: time.Hour}
	if err := recorder.Record(context.Background(), record); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var contractAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT contract_not_before
		FROM sage.migration_run WHERE evidence_id = $1`, record.EvidenceID).
		Scan(&contractAt); err != nil {
		t.Fatalf("read contract_not_before: %v", err)
	}
	if contractAt == nil || !contractAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("contract_not_before = %v, want %v", contractAt, now.Add(time.Hour))
	}
}
