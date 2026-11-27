package approvalcard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/store"
)

// tuning-05b on a real database: the loader reads the finding's stored
// hint_evidence and the card carries it. The golden is the card JSON the
// dashboard's mocked e2e (web/e2e/approval-hint-plans.spec.ts) copies.

const hintCardGolden = "testdata/hint_evidence_card.golden.json"

// queueHintAction inserts an open query_tuning finding with detail and its
// queued apply_query_hint proposal; it returns the queue and finding ids.
func queueHintAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	detail string) (int, int) {
	t.Helper()
	n := time.Now().UnixNano()
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, status)
		VALUES ('query_tuning', 'warning', 'query', $1, 'Hint for query 4242', $2::jsonb,
		'Pin the index scan', 'open') RETURNING id`, fmt.Sprintf("queryid:%d", n),
		detail).Scan(&findingID); err != nil {
		t.Fatalf("insert hint finding: %v", err)
	}
	sql := fmt.Sprintf("INSERT INTO hint_plan.hints (query_id, application_name, hints) "+
		"VALUES (%d, '', 'IndexScan(orders idx_orders_customer)')", n)
	id, err := store.NewActionStore(pool).ProposeWithMetadata(ctx, nil, findingID, sql,
		fmt.Sprintf("DELETE FROM hint_plan.hints WHERE query_id = %d", n), "moderate",
		store.ActionProposalMetadata{ActionType: "apply_query_hint",
			PolicyDecision: "queue_approval", Guardrails: []string{"approval_required"}})
	if err != nil {
		t.Fatalf("queue hint action: %v", err)
	}
	return id, findingID
}

// goldenView is the part of a card's JSON the golden pins.
func goldenView(t *testing.T, c Card) map[string]any {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, key := range []string{"action_type", "status", "title", "hint_evidence"} {
		out[key] = all[key]
	}
	return out
}

func TestHintEvidence_LoaderCardCarriesTheStoredComparison(t *testing.T) {
	pool, ctx := livePool(t)
	detail := `{"queryid":4242,"hint_evidence":` +
		changedEvidenceJSON(plainPlanJSON, hintedPlanJSON) + `}`
	queueID, findingID := queueHintAction(t, ctx, pool, detail)
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"}
	c, err := l.Card(ctx, queueID)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if c.QueueID != queueID || c.Finding == nil || c.Finding.ID != findingID ||
		c.ActionType != "apply_query_hint" {
		t.Fatalf("card head = %+v", c)
	}
	h := c.HintEvidence
	if h == nil || h.Verdict != "changed" || h.PlainCost == nil || *h.PlainCost != 412 ||
		h.HintedCost == nil || *h.HintedCost != 254.6 {
		t.Fatalf("hint evidence = %+v", h)
	}
	assertJSONEqual(t, "plain_plan", h.PlainPlan, plainPlanJSON)
	assertJSONEqual(t, "hinted_plan", h.HintedPlan, hintedPlanJSON)
	got := goldenView(t, c)
	if os.Getenv("SAGE_UPDATE_GOLDEN") == "1" {
		raw, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(hintCardGolden, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	raw, err := os.ReadFile(hintCardGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("golden: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("card JSON differs from %s:\n%s", hintCardGolden, g)
	}
	assertPendingCarriesEvidence(t, ctx, l, queueID)
}

func assertPendingCarriesEvidence(t *testing.T, ctx context.Context, l Loader, queueID int) {
	t.Helper()
	cards, err := l.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, c := range cards {
		if c.QueueID == queueID {
			if c.HintEvidence == nil || c.HintEvidence.Verdict != "changed" {
				t.Fatalf("pending card hint evidence = %+v", c.HintEvidence)
			}
			return
		}
	}
	t.Fatalf("queue item %d is not pending", queueID)
}

func TestHintEvidence_LoaderMalformedEvidenceStillRendersTheCard(t *testing.T) {
	pool, ctx := livePool(t)
	bad := strings.Replace(changedEvidenceJSON(plainPlanJSON, hintedPlanJSON),
		`"plain_cost":412.0`, `"plain_cost":"412.0"`, 1)
	queueID, findingID := queueHintAction(t, ctx, pool, `{"hint_evidence":`+bad+`}`)
	buf := captureWarnings(t)
	c, err := Loader{Pool: pool, Database: "orders"}.Card(ctx, queueID)
	if err != nil {
		t.Fatalf("a malformed hint_evidence failed the card: %v", err)
	}
	if c.QueueID != queueID || c.HintEvidence != nil || c.SQL == "" {
		t.Fatalf("card = %+v", c)
	}
	warns := hintWarnings(buf)
	if len(warns) != 1 || !strings.Contains(warns[0], fmt.Sprintf("finding_id=%d", findingID)) {
		t.Fatalf("want one WARN naming finding %d, got %q", findingID, warns)
	}
}

// A loader read error fails the card exactly as before: no card without
// evidence. A finding detail that is not a JSON object cannot be decoded.
func TestHintEvidence_LoaderReadErrorStillFailsTheCard(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, findingID := queueHintAction(t, ctx, pool,
		`[{"hint_evidence":{"verdict":"changed"}}]`)
	// The undecodable finding would fail every later Pending read.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE sage.findings SET status = 'resolved',
			resolved_at = now() WHERE id = $1`, findingID)
	})
	c, err := Loader{Pool: pool, Database: "orders"}.Card(ctx, queueID)
	if err == nil || !strings.Contains(err.Error(), "decode finding detail") {
		t.Fatalf("err = %v, want the finding detail decode error", err)
	}
	if c.QueueID != 0 || c.HintEvidence != nil || c.SQL != "" {
		t.Fatalf("a failed read produced a card: %+v", c)
	}
}

// After tuning-05 retires a hint its finding is resolved, and the queued
// card for it is no longer pending (existing behaviour, re-asserted).
func TestHintEvidence_ResolvedHintFindingHidesThePendingCard(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, findingID := queueHintAction(t, ctx, pool, `{"hint_evidence":`+
		changedEvidenceJSON(plainPlanJSON, hintedPlanJSON)+`}`)
	l := Loader{Pool: pool, Database: "orders"}
	assertPendingCarriesEvidence(t, ctx, l, queueID)
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE id = $1`, findingID); err != nil {
		t.Fatal(err)
	}
	cards, err := l.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, c := range cards {
		if c.QueueID == queueID {
			t.Fatalf("the retired hint's card is still pending: %+v", c)
		}
	}
}
