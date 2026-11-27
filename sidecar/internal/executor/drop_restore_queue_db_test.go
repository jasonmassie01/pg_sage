package executor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// executor-01 review: queue hygiene never closes a withheld restore card
// because another index covers its definition. A redundant-index drop is
// covered by design, and the regression says the planner wants it back.
func TestDropRestoreCardSurvivesTheCoverageSweep(t *testing.T) {
	f := newRestoreFixture(t, false)
	if err := SetEmergencyStop(f.ctx, f.pool, true, "test"); err != nil {
		t.Fatal(err)
	}
	f.monitor()
	card := f.restoreCard(t)
	mustExec(t, f.pool, f.ctx, "CREATE INDEX "+f.table+"_dup ON public."+f.table+" (a)")
	control := f.coveredControlProposal(t)

	if _, err := f.e.supersedeStaleApprovals(f.ctx); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if again := f.restoreCard(t); again.ID != card.ID {
		t.Fatalf("restore card = %d, want the original %d still pending", again.ID, card.ID)
	}
	if outcome, reason := f.outcome(t); outcome != "rollback_withheld" {
		t.Fatalf("outcome = %q (%s), want rollback_withheld", outcome, reason)
	}
	var status, reason string
	if err := f.pool.QueryRow(f.ctx, `SELECT status, COALESCE(reason, '')
		FROM sage.action_queue WHERE id = $1`, control).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || !strings.Contains(reason, "already covers") {
		t.Fatalf("control create = %s (%s), want superseded as covered", status, reason)
	}
}

// coveredControlProposal queues an ordinary index create of the dropped
// definition under a new name (open finding), which the sweep must close
// as covered: proof that the sweep ran over this table.
func (f restoreFixture) coveredControlProposal(t *testing.T) int {
	t.Helper()
	var findingID int
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('missing_index', 'info', 'table', $1, 'control', '{}'::jsonb, 'open')
		RETURNING id`, "public."+f.table).Scan(&findingID); err != nil {
		t.Fatalf("insert control finding: %v", err)
	}
	sql := "CREATE INDEX CONCURRENTLY " + f.table + "_ctl ON public." + f.table +
		" USING btree (a)"
	id, err := store.NewActionStore(f.pool).ProposeWithMetadata(f.ctx, nil, findingID, sql,
		"", "safe", store.ActionProposalMetadata{ActionType: "create_index_concurrently",
			PolicyDecision: PolicyDecisionQueueApproval})
	if err != nil {
		t.Fatalf("queue control create: %v", err)
	}
	return id
}
