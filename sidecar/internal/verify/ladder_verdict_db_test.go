package verify

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// linkLadderAction inserts an action_log row and links queueID to it.
func linkLadderAction(t *testing.T, pool *pgxpool.Pool, queueID int) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `INSERT INTO sage.action_log (action_type,
		sql_executed, outcome) VALUES ('drop_index', 'DROP INDEX x', 'monitoring')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "UPDATE sage.action_queue SET action_log_id = NULL "+
			"WHERE action_log_id = $1", id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE id = $1", id)
	})
	if _, err := pool.Exec(t.Context(), `UPDATE sage.action_queue SET action_log_id = $1
		WHERE id = $2`, id, queueID); err != nil {
		t.Fatalf("link queue row: %v", err)
	}
	return id
}

// A verdict recorded again replaces the action's earlier tier-3 record;
// the records before it, and their order, are kept.
func TestRecordVerdictForAction_ReplacesTheEarlierVerdict(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	queueID := insertLadderQueueRow(t, pool)
	actionID := linkLadderAction(t, pool, queueID)
	now := time.Now()
	if err := AppendLadder(ctx, pool, queueID,
		FromWhatIf(WhatIfEvidence{Verdict: "verified"}, now),
		FromRehearsal(Rehearsal{Passed: true, ReceiptID: 7}, now)); err != nil {
		t.Fatalf("append: %v", err)
	}
	for _, verdict := range []string{OutcomeRegressed, OutcomeInsufficient, OutcomeNeutral} {
		n, err := RecordVerdictForAction(ctx, pool, actionID,
			FromVerdict(verdict, nil, actionID, now))
		if err != nil || n != 1 {
			t.Fatalf("record %s: rows %d, err %v; want 1 row", verdict, n, err)
		}
	}
	raw, err := ReadLadder(ctx, pool, queueID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	recs, err := ParseLadder(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	if len(recs) != 3 || recs[0].Tier != TierPlanOnly || recs[1].Tier != TierClone {
		t.Fatalf("ladder = %s, want tier 1, tier 2 and one tier 3", raw)
	}
	if recs[2].Tier != TierCanary || recs[2].Evidence["verdict"] != OutcomeNeutral ||
		!recs[2].Passed() {
		t.Fatalf("tier 3 = %+v, want the latest verdict (neutral, passed)", recs[2])
	}
}

// Nil/empty and invalid input: a NULL ladder gets just the verdict; a
// stored value that is not an array is kept, not discarded; an action no
// queue row ran updates nothing.
func TestRecordVerdictForAction_NullNonArrayAndNoQueueRow(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	nullRow := insertLadderQueueRow(t, pool)
	nullAction := linkLadderAction(t, pool, nullRow)
	rec := FromVerdict(OutcomeImproved, nil, nullAction, time.Now())
	if n, err := RecordVerdictForAction(ctx, pool, nullAction, rec); err != nil || n != 1 {
		t.Fatalf("NULL ladder: rows %d, err %v", n, err)
	}
	raw, _ := ReadLadder(ctx, pool, nullRow)
	if recs, err := ParseLadder(raw); err != nil || len(recs) != 1 ||
		recs[0].Tier != TierCanary {
		t.Fatalf("NULL ladder became %s (%v), want one tier-3 record", raw, err)
	}
	objRow := insertLadderQueueRow(t, pool)
	objAction := linkLadderAction(t, pool, objRow)
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET ladder = '{"x":1}'
		WHERE id = $1`, objRow); err != nil {
		t.Fatal(err)
	}
	if n, err := RecordVerdictForAction(ctx, pool, objAction, rec); err != nil || n != 1 {
		t.Fatalf("non-array ladder: rows %d, err %v", n, err)
	}
	var kept bool
	if err := pool.QueryRow(ctx, `SELECT ladder->0 = '{"x":1}'::jsonb
		AND jsonb_array_length(ladder) = 2 FROM sage.action_queue WHERE id = $1`,
		objRow).Scan(&kept); err != nil || !kept {
		t.Fatalf("non-array ladder not kept beside the verdict: %v", err)
	}
	if n, err := RecordVerdictForAction(ctx, pool, -42, rec); err != nil || n != 0 {
		t.Fatalf("action without a queue row: rows %d, err %v; want 0, nil", n, err)
	}
}
