package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// mcp-06: sage.source_fix gains deploy_reported_at, the time pg_sage heard
// of a deploy, so a deploy reported after its verify window closed can be
// told from a live one. Rows reported before the column have it null.

func TestSourceFixDeployReportedAtMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requireDeployReportedAt(t, ctx, pool)
	if _, err := pool.Exec(ctx,
		`ALTER TABLE sage.source_fix DROP COLUMN IF EXISTS deploy_reported_at`); err != nil {
		t.Fatalf("simulating the pre-mcp-06 schema: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.source_fix (finding_id, packet_hash,
		stage, reported_by, deployed_at) VALUES (2147480001, 'h', 'deployed', 'agent:x',
		now() - interval '1 hour') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("inserting a pre-migration report: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.source_fix WHERE id = $1`, id)
	})
	bootstrapWithRetry(t, ctx, pool)
	requireDeployReportedAt(t, ctx, pool)
	var reported *string
	if err := pool.QueryRow(ctx, `SELECT deploy_reported_at::text FROM sage.source_fix
		WHERE id = $1`, id).Scan(&reported); err != nil {
		t.Fatalf("reading the pre-migration report: %v", err)
	}
	if reported != nil {
		t.Fatalf("pre-migration report has deploy_reported_at %s, want NULL", *reported)
	}
	bootstrapWithRetry(t, ctx, pool) // a second run is a no-op
	requireDeployReportedAt(t, ctx, pool)
}

func TestSourceFixDeployReportedAtIsATrancheTwoStep(t *testing.T) {
	if !isTrancheTwoStep("ddlSourceFixDeployReportedAt") {
		t.Fatalf("tranche two steps = %v, want ddlSourceFixDeployReportedAt among them",
			trancheTwoMigrations())
	}
	found := false
	for _, s := range migrationSteps() {
		found = found || s.name == "ddlSourceFixDeployReportedAt"
	}
	if !found {
		t.Fatal("migrationSteps does not run ddlSourceFixDeployReportedAt")
	}
}

func requireDeployReportedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var typ string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute WHERE attrelid = 'sage.source_fix'::regclass
		  AND attname = 'deploy_reported_at' AND NOT attisdropped`).
		Scan(&typ, &notNull); err != nil {
		t.Fatalf("reading sage.source_fix.deploy_reported_at: %v", err)
	}
	if typ != "timestamp with time zone" || notNull {
		t.Fatalf("deploy_reported_at is %s (not null %v), want nullable timestamptz",
			typ, notNull)
	}
}
