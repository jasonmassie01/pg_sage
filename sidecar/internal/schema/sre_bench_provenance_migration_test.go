package schema

import (
	"context"
	"testing"
)

// Roadmap 1.1 (2026-10-03): a stored bench or game-day report keeps its
// provenance: origin (signed_release, local_run, operator, game_day), the
// pg_sage build it scored and, when signed, the verified signature.
// Additive and idempotent on top of ddlSREAutonomy; reports stored before
// it are operator reports (bench) or game days.

const provDeployment = "71717171-7171-4171-8171-717171717171"

func provCleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := requireDB(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.sre_eval_runs WHERE deployment_id = $1", provDeployment)
	}
	clean()
	t.Cleanup(clean)
}

// insertRun stores a minimal report; origin and signature are optional
// ("" and nil leave the column default).
func insertRun(ctx context.Context, t *testing.T, id, source, origin string,
	signature any) error {
	t.Helper()
	pool, _ := requireDB(t)
	var db any
	if source == "game_day" {
		db = "orders"
	}
	sha := id[:2]
	if origin == "" {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_eval_runs
			(deployment_id, id, source, schema_version, generated_at, ingested_by,
			 database_name, report_sha256, cells)
			VALUES ($1, $2, $3, 'v1', now(), 'test', $4, sha256($5::bytea), '[]')`,
			provDeployment, id, source, db, sha)
		return err
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.sre_eval_runs
		(deployment_id, id, source, schema_version, generated_at, ingested_by,
		 database_name, report_sha256, cells, origin, signature)
		VALUES ($1, $2, $3, 'v1', now(), 'test', $4, sha256($5::bytea), '[]', $6, $7)`,
		provDeployment, id, source, db, sha, origin, signature)
	return err
}

func TestSREMigrationBenchProvenance_Columns(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	provCleanup(t, ctx)
	const id = "11111111-1111-4111-8111-111111111111"
	if err := insertRun(ctx, t, id, "bench", "", nil); err != nil {
		t.Fatal(err)
	}
	var origin, version, commit string
	var signed bool
	if err := pool.QueryRow(ctx, `SELECT origin, pg_sage_version, pg_sage_commit,
		signature IS NOT NULL FROM sage.sre_eval_runs WHERE deployment_id = $1 AND id = $2`,
		provDeployment, id).Scan(&origin, &version, &commit, &signed); err != nil {
		t.Fatal(err)
	}
	if origin != "operator" || version != "" || commit != "" || signed {
		t.Fatalf("defaults = %q %q %q %v, want an unsigned operator report", origin, version,
			commit, signed)
	}
}

func TestSREMigrationBenchProvenance_Constraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	provCleanup(t, ctx)
	sig := `{"identity":"ci","issuer":"gh"}`
	ok := map[string]struct{ source, origin string }{
		"21111111-1111-4111-8111-111111111111": {"bench", "local_run"},
		"31111111-1111-4111-8111-111111111111": {"bench", "operator"},
		"41111111-1111-4111-8111-111111111111": {"game_day", "game_day"},
	}
	for id, c := range ok {
		if err := insertRun(ctx, t, id, c.source, c.origin, nil); err != nil {
			t.Errorf("%s/%s refused: %v", c.source, c.origin, err)
		}
	}
	if err := insertRun(ctx, t, "51111111-1111-4111-8111-111111111111", "bench",
		"signed_release", sig); err != nil {
		t.Errorf("signed release with a signature refused: %v", err)
	}
	bad := map[string]struct {
		source, origin string
		sig            any
	}{
		"an unknown origin":          {"bench", "trusted", nil},
		"signed without a signature": {"bench", "signed_release", nil},
		"a signature on an operator": {"bench", "operator", sig},
		"a signature on a local run": {"bench", "local_run", sig},
	}
	n := 6
	for name, c := range bad {
		id := string(rune('0'+n)) + "1111111-1111-4111-8111-111111111111"
		n++
		if err := insertRun(ctx, t, id, c.source, c.origin, c.sig); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Game days stored before the migration are marked game days when it
// runs (again); bench reports stay operator reports.
func TestSREMigrationBenchProvenance_BackfillsGameDays(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	provCleanup(t, ctx)
	const gd, bench = "a1111111-1111-4111-8111-111111111111",
		"b1111111-1111-4111-8111-111111111111"
	for id, source := range map[string]string{gd: "game_day", bench: "bench"} {
		if err := insertRun(ctx, t, id, source, "operator", nil); err != nil {
			t.Fatal(err)
		}
	}
	bootstrapWithRetry(t, ctx, pool)
	for id, want := range map[string]string{gd: "game_day", bench: "operator"} {
		var origin string
		if err := pool.QueryRow(ctx, `SELECT origin FROM sage.sre_eval_runs
			WHERE deployment_id = $1 AND id = $2`, provDeployment, id).Scan(&origin); err != nil {
			t.Fatal(err)
		}
		if origin != want {
			t.Errorf("%s origin = %q after the migration, want %q", id, origin, want)
		}
	}
}
