package approvalcard

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 1.4: the approval card says when its action's class has shadow
// history (what pg_sage would have done below its earned level, and how
// that scored), and whether pg_sage had recorded this very proposal as a
// shadow decision, so the operator's decision is known to score it.
// Shadow history is not part of the card hash: it changes as decisions
// are scored, the action does not.

func TestCardShowsShadowHistoryOfItsClass(t *testing.T) {
	pool, ctx := livePool(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.shadow_decision`); err != nil {
		t.Fatal(err)
	}
	queueID, findingID := queueOptimizerIndex(t, ctx, pool)
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"}
	c, err := l.Card(ctx, queueID)
	if err != nil {
		t.Fatal(err)
	}
	if c.ShadowHistory != nil {
		t.Fatalf("no shadow decisions yet, card shows %+v", c.ShadowHistory)
	}
	hash := c.CardHash
	var sql string
	if err := pool.QueryRow(ctx, `SELECT proposed_sql FROM sage.action_queue WHERE id = $1`,
		queueID).Scan(&sql); err != nil {
		t.Fatal(err)
	}
	s := shadow.NewStore(pool)
	record := func(sql, score string, finding int64) {
		t.Helper()
		d, ok, err := s.Record(ctx, shadow.Decision{Database: "orders",
			Fingerprint: shadow.Fingerprint("index_create", "public.orders", shadow.Shape(sql)),
			Family:      "tuning", Class: "index_create", FindingID: finding, Title: "x",
			Object: "public.orders", SQL: sql, Shape: shadow.Shape(sql),
			Prediction:  verify.NoPrediction("index_create", "test"),
			GateVerdict: "queue_approval", GateReason: "approval_required",
			TrustedVerdict: "execute", TrustedReason: "autonomy_l3", GrantedLevel: 1})
		if err != nil || !ok {
			t.Fatalf("record: %v %v", ok, err)
		}
		if score != "" {
			if _, err := pool.Exec(ctx, `UPDATE sage.shadow_decision SET status = 'scored',
				score = $2, score_source = 'hypopg', counted = true, scored_at = now()
				WHERE id = $1`, d.ID, score); err != nil {
				t.Fatal(err)
			}
		}
	}
	record(`CREATE INDEX CONCURRENTLY o1 ON public.orders (a)`, "correct", 1)
	record(`CREATE INDEX CONCURRENTLY o2 ON public.orders (b)`, "incorrect", 2)
	record(sql, "", int64(findingID))
	c, err = l.Card(ctx, queueID)
	if err != nil {
		t.Fatal(err)
	}
	h := c.ShadowHistory
	if h == nil || h.Class != "index_create" || h.Family != "tuning" ||
		h.Summary.Total != 3 || h.Summary.Correct != 1 || h.Summary.Incorrect != 1 ||
		h.Summary.Pending != 1 || h.ThisProposal == nil || h.ThisProposal.SQL != sql {
		t.Fatalf("shadow history %+v", h)
	}
	if c.CardHash != hash {
		t.Fatal("shadow history changed the card hash")
	}
}
