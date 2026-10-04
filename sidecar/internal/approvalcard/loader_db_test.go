package approvalcard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// The loader reads a card's inputs from the database: the queue row, its
// finding, the gate decision linked to it, an earlier operator rejection
// of the same SQL, and the snooze.

func TestLoaderCardFromTheDatabase(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, findingID := queueOptimizerIndex(t, ctx, pool)
	var sql string
	_ = pool.QueryRow(ctx, `SELECT proposed_sql FROM sage.action_queue WHERE id = $1`,
		queueID).Scan(&sql)
	var decisionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, guardrails, evidence, evidence_id,
		queue_id) VALUES ('optimizer', 'create_index', '["public.orders"]',
		'queue_approval', 'moderate', 'trust_ramp_not_satisfied', '["approval_required"]',
		'{}', $2, $1) RETURNING id`, queueID, fmt.Sprintf("ev-loader-%d", queueID)).
		Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	// An earlier proposal of the same SQL that an operator rejected.
	old, err := store.NewActionStore(pool).Propose(ctx, nil, findingID, sql, "", "moderate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET status = 'rejected',
		decided_by = 1, decided_at = now() - interval '1 day',
		reason = 'app owns this index' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"}
	c, err := l.Card(ctx, queueID)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if c.QueueID != queueID || c.Database != "orders" || c.Finding == nil ||
		c.Finding.ID != findingID || c.Title != "Index recommendation for public.orders" ||
		c.Predicted.ImprovementPct == nil || *c.Predicted.ImprovementPct != 42.5 ||
		c.Rationale == nil || c.Rationale.Source != "llm" {
		t.Fatalf("card = %+v", c)
	}
	for _, code := range []string{"gate:trust_ramp_not_satisfied", "what_if_unverified",
		"operator_rejected_before", "trust_level"} {
		if !hasCode(c, code) {
			t.Fatalf("why %v lacks %s", reasonCodes(c), code)
		}
	}
	if !strings.Contains(textOf(c.Why), "app owns this index") ||
		!strings.Contains(evidenceLabels(c), "decision:") {
		t.Fatalf("why %+v evidence %s", c.Why, evidenceLabels(c))
	}
	if c.Rollback.Class != "reversible" || c.Risk.Tier != "moderate" || c.CardHash == "" {
		t.Fatalf("rollback %+v risk %+v", c.Rollback, c.Risk)
	}
	_ = decisionID
}

func TestLoaderCardNotFoundAndSnoozed(t *testing.T) {
	pool, ctx := livePool(t)
	l := Loader{Pool: pool, Database: "orders"}
	if _, err := l.Card(ctx, 987654321); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing card: err = %v, want ErrNotFound", err)
	}
	if _, err := (Loader{}).Card(ctx, 1); err == nil {
		t.Fatal("a loader without a pool produced a card")
	}
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	until := time.Now().Add(2 * time.Hour)
	if err := store.NewActionStore(pool).Snooze(ctx, queueID, 3, until,
		"after the release"); err != nil {
		t.Fatal(err)
	}
	c, err := l.Card(ctx, queueID)
	if err != nil || c.SnoozedUntil == nil || c.SnoozeReason != "after the release" {
		t.Fatalf("snoozed card = %+v, %v", c, err)
	}
}

func TestLoaderPendingListsEveryPendingItem(t *testing.T) {
	pool, ctx := livePool(t)
	a, _ := queueOptimizerIndex(t, ctx, pool)
	b, _ := queueOptimizerIndex(t, ctx, pool)
	cards, err := Loader{Pool: pool, Database: "orders"}.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, c := range cards {
		seen[c.QueueID] = true
		if c.Database != "orders" || len(c.Why) == 0 {
			t.Fatalf("card = %+v", c)
		}
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("pending cards %v lack %d or %d", seen, a, b)
	}
}

func TestLinkedDecisionIsTheNewest(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	for i, reason := range []string{"approval_required", "outside_maintenance_window"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.decision (feature, intent,
			target_objects, verdict, risk_tier, reason, evidence, evidence_id, queue_id)
			VALUES ('optimizer', 'x', '[]', 'queue_approval', 'moderate', $1, '{}',
			$3, $2)`,
			reason, queueID, fmt.Sprintf("ev-newest-%d-%d", queueID, i)); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Loader{Pool: pool, Database: "orders"}.Card(ctx, queueID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(c, "gate:outside_maintenance_window") || hasCode(c, "gate:approval_required") {
		t.Fatalf("why = %v", reasonCodes(c))
	}
}
