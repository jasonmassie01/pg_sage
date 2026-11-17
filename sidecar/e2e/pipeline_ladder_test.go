//go:build e2e

package e2e

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-03a: a drop is costly to revert, so it runs unattended only with a
// passed tier-2 (clone rehearsal) record on its finding; without one the
// standing gate queues it for approval at every trust level.

// rehearsedLadder is a finding's ladder holding one passed clone rehearsal.
func rehearsedLadder() []any {
	return []any{map[string]any{"tier": float64(verify.TierClone), "name": "clone",
		"method": verify.MethodLadderRehearsal, "result": verify.LadderPassed,
		"at": "2026-11-16T09:00:00Z"}}
}

// shapeUnrehearsedDropWaits drives the B06 duplicate drop without a clone
// rehearsal: the decision queues it for approval and the index stays.
func shapeUnrehearsedDropWaits(t *testing.T, pool *pgxpool.Pool, r *checkReport) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b06n;
		CREATE TABLE shp_b06n (s text);
		CREATE INDEX shp_b06n_dup_a ON shp_b06n (s);
		CREATE INDEX shp_b06n_dup_b ON shp_b06n (s);`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "duplicate_index", Severity: "warning",
		ObjectType: "index", ObjectIdentifier: "public.shp_b06n_dup_a",
		Title:          "duplicate of shp_b06n_dup_b (no clone rehearsal)",
		RecommendedSQL: "DROP INDEX CONCURRENTLY public.shp_b06n_dup_a;",
		RollbackSQL:    "CREATE INDEX CONCURRENTLY shp_b06n_dup_a ON public.shp_b06n (s)",
		ActionRisk:     "safe",
	})
	gate := quickScalar(pool, `SELECT verdict || '/' || reason FROM sage.decision
		WHERE target_objects ? 'public.shp_b06n_dup_a' ORDER BY id DESC LIMIT 1`)
	_, _, exists := indexAccessMethod(t, pool, "shp_b06n_dup_a")
	r.add(t, "CHECK-B06c", gate == "queue_approval/approval_required" && exists,
		"an unrehearsed drop waits for approval and the index stays (gate="+gate+")")
}
