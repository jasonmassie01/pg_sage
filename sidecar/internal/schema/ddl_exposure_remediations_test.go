package schema

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fleetpol-02b: sage.exposure_remediations records what an exposure revoke
// took away (the prior ACL and the revoked privileges), keyed by its
// action_log row. One idempotent T6 step under lock_timeout.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestExposureRemediationsStepIsRegisteredAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := -1
	for i, step := range steps {
		if step[0] == "ddlExposureRemediations" {
			if at >= 0 {
				t.Fatalf("ddlExposureRemediations listed twice: %v", steps)
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("trancheSixMigrations = %v, want ddlExposureRemediations", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.exposure_remediations",
		"action_log_id", "object", "prior_acl jsonb", "revoked jsonb", "created_at",
		"CREATE INDEX IF NOT EXISTS idx_exposure_remediations_object"} {
		if !strings.Contains(ddlExposureRemediations, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlExposureRemediations) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not prechecked; it would run on every boot", stmt.sql)
		}
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server, the step
// re-runs on an existing table, a row round-trips and a second row for
// the same action is refused.
func TestExposureRemediationsBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "exposure_remediations")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, ddlExposureRemediations); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-run of the step: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome) VALUES ('revoke_public_grant', 'REVOKE SELECT ON TABLE public.t FROM PUBLIC',
		'monitoring') RETURNING id`).Scan(&actionID); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	insert := `INSERT INTO sage.exposure_remediations (action_log_id, object, prior_acl,
		revoked) VALUES ($1, 'public.t', '["=r/o"]', '["SELECT"]')`
	if _, err := pool.Exec(ctx, insert, actionID); err != nil {
		t.Fatalf("insert remediation: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, actionID); err == nil {
		t.Fatal("a second remediation row for one action was accepted")
	}
	var revoked string
	var created bool
	if err := pool.QueryRow(ctx, `SELECT revoked::text, created_at IS NOT NULL
		FROM sage.exposure_remediations WHERE action_log_id = $1`, actionID).
		Scan(&revoked, &created); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if revoked != `["SELECT"]` || !created {
		t.Fatalf("row = %s created=%v", revoked, created)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.exposure_remediations (action_log_id,
		object, prior_acl, revoked) VALUES ($1, '', '[]', '[]')`, actionID+1000000); err == nil {
		t.Fatal("an empty object was accepted")
	}
}
