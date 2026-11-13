package runtime

import (
	"context"
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
	// Qualified as the planner emits it: executor DDL runs with search_path
	// pinned to pg_catalog, pg_temp, so a bare name no longer resolves.
	const table = "public.migration_rehearsal_clone_probe"
	_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table+"; CREATE TABLE "+table+
		" (id bigint NOT NULL)")
	if err != nil {
		t.Fatalf("create rehearsal fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	// migration-07: Run now refuses without a production identity; this
	// clone is checked against a production server it is not.
	runner := NewPostgresRehearsalRunner(time.Second, 10*time.Second).
		WithProduction(func(context.Context) (ServerIdentity, error) {
			return ServerIdentity{Host: "production.invalid", Port: 5432,
				StartedAt: time.Unix(1, 0), ServerPort: 5432}, nil
		})
	measurement, err := runner.Run(ctx, testdb.SkipUnlessLive(t), planpkg.Plan{
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
	if measurement.MaxStepDuration <= 0 {
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
		ContractDelay: time.Hour,
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

// migration-03: a conflict on evidence_id refreshes source_sql_hash and
// reason, so the row describes the latest run instead of the first one.
func TestPostgresRecorderRefreshesHashAndReasonOnConflict(t *testing.T) {
	pool := migrationRuntimePool(t)
	ctx := context.Background()
	recorder := NewPostgresRecorder(pool)
	first := Record{Request: request(), Verdict: VerdictParked,
		EvidenceID: "ev_runtime_refresh", Reason: "rehearsal lock ceiling exceeded"}
	if err := recorder.Record(ctx, first); err != nil {
		t.Fatalf("Record(first): %v", err)
	}
	second := first
	second.Request.SQL = "ALTER TABLE public.users ADD CONSTRAINT users_name_key UNIQUE (name)"
	second.Verdict = VerdictRecommendOnly
	second.Reason = "operator approval required"
	if err := recorder.Record(ctx, second); err != nil {
		t.Fatalf("Record(second): %v", err)
	}
	var count int
	var verdict, hash, reason string
	err := pool.QueryRow(ctx, `SELECT count(*) OVER (), verdict, source_sql_hash, reason
		FROM sage.migration_run WHERE evidence_id=$1`, first.EvidenceID).Scan(
		&count, &verdict, &hash, &reason)
	if err != nil {
		t.Fatalf("read migration continuation: %v", err)
	}
	if count != 1 || verdict != "recommend_only" {
		t.Fatalf("rows=%d verdict=%q, want one recommend_only row", count, verdict)
	}
	if hash != migrationHash(second.Request.SQL) || hash == migrationHash(first.Request.SQL) {
		t.Fatalf("source_sql_hash=%q, want the second run's hash", hash)
	}
	if reason != second.Reason {
		t.Fatalf("reason=%q, want %q", reason, second.Reason)
	}
}
