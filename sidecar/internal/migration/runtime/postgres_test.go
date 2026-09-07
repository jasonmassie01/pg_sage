package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestPostgresRehearsalRunnerExecutesExpandOnlyOnCloneDSN(t *testing.T) {
	pool := migrationRuntimePool(t)
	ctx := context.Background()
	const table = "migration_rehearsal_clone_probe"
	_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table+"; CREATE TABLE "+table+
		" (id bigint NOT NULL)")
	if err != nil {
		t.Fatalf("create rehearsal fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	runner := NewPostgresRehearsalRunner(time.Second, 10*time.Second)
	measurement, err := runner.Run(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"), planpkg.Plan{
		ExpandSteps: []planpkg.Step{{
			Kind: planpkg.StepCreateUniqueIndex, RequiresTopLevel: true,
			SQL: "CREATE UNIQUE INDEX CONCURRENTLY migration_rehearsal_probe_idx ON " +
				table + " (id)",
		}},
		ContractSteps: []planpkg.Step{{
			Kind: planpkg.StepAttachUniqueConstraint,
			SQL: "ALTER TABLE " + table + " ADD CONSTRAINT never_same_cycle UNIQUE " +
				"USING INDEX migration_rehearsal_probe_idx",
		}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if measurement.MaxLockDuration <= 0 {
		t.Fatalf("measurement = %#v", measurement)
	}
	var indexExists, constraintExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname='never_same_cycle')`,
		"migration_rehearsal_probe_idx").Scan(&indexExists, &constraintExists); err != nil {
		t.Fatalf("verify rehearsal state: %v", err)
	}
	if !indexExists || constraintExists {
		t.Fatalf("index=%v contract=%v", indexExists, constraintExists)
	}
}

func TestPostgresApplierExecutesTopLevelAndTransactionalSteps(t *testing.T) {
	pool := migrationRuntimePool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE public.migration_runtime_users (`+
		`id bigint PRIMARY KEY, email text)`)
	if err != nil {
		t.Fatalf("create fixture table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TABLE IF EXISTS public.migration_runtime_users CASCADE`)
	})
	applier := NewPostgresApplier(pool, time.Second, 10*time.Second)
	steps := []planpkg.Step{
		{Kind: planpkg.StepCreateUniqueIndex, RequiresTopLevel: true,
			SQL: `CREATE UNIQUE INDEX CONCURRENTLY migration_runtime_email_idx ` +
				`ON public.migration_runtime_users(email)`},
		{Kind: planpkg.StepAttachUniqueConstraint,
			SQL: `ALTER TABLE public.migration_runtime_users ADD CONSTRAINT ` +
				`migration_runtime_email_key UNIQUE USING INDEX migration_runtime_email_idx`},
	}
	for _, step := range steps {
		if err := applier.Apply(ctx, step); err != nil {
			t.Fatalf("Apply(%s): %v", step.Kind, err)
		}
	}
	var exists bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conname='migration_runtime_email_key')`).Scan(&exists)
	if err != nil || !exists {
		t.Fatalf("constraint exists=%v err=%v", exists, err)
	}
}

func TestPostgresRecorderPersistsMigrationContinuation(t *testing.T) {
	pool := migrationRuntimePool(t)
	recorder := NewPostgresRecorder(pool)
	record := Record{
		Request: request(), Verdict: VerdictExpanded, EvidenceID: "ev_runtime_record",
		ContractNotBeforeCycle: 2,
	}
	if err := recorder.Record(context.Background(), record); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var phase, verdict, hash string
	var contractAt *time.Time
	err := pool.QueryRow(context.Background(), `SELECT phase, verdict,
		source_sql_hash, contract_not_before FROM sage.migration_run
		WHERE evidence_id=$1`, record.EvidenceID).Scan(
		&phase, &verdict, &hash, &contractAt,
	)
	if err != nil {
		t.Fatalf("read migration continuation: %v", err)
	}
	if phase != "expand" || verdict != "promote" || hash == "" || contractAt == nil {
		t.Fatalf("stored phase=%q verdict=%q hash=%q contract=%v",
			phase, verdict, hash, contractAt)
	}
}

func migrationRuntimePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return pool
}
