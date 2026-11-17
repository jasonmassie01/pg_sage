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
	"github.com/pg-sage/sidecar/internal/verify"
)

// ladderCardGolden is the card JSON the dashboard's mocked e2e
// (web/e2e/approval-ladder.spec.ts) copies. SAGE_UPDATE_GOLDEN=1 rewrites
// it.
const ladderCardGolden = "testdata/ladder_card.golden.json"

// queueLadderDrop inserts an open unused-index finding and its queued drop.
func queueLadderDrop(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	n := time.Now().UnixNano()
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, status)
		VALUES ('unused_index', 'info', 'index', $1, 'Unused index public.idx_old',
		'{}'::jsonb, 'Drop the unused index', 'open') RETURNING id`,
		fmt.Sprintf("public.idx_old_%d", n)).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	sql := fmt.Sprintf(`DROP INDEX CONCURRENTLY "public"."idx_old_%d"`, n)
	id, err := store.NewActionStore(pool).ProposeWithMetadata(ctx, nil, findingID, sql,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_old_%d ON public.t (a)", n), "moderate",
		store.ActionProposalMetadata{ActionType: "drop_unused_index",
			PolicyDecision: "queue_approval", Guardrails: []string{"approval_required"}})
	if err != nil {
		t.Fatalf("queue drop: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_queue WHERE id = $1", id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id = $1", findingID)
	})
	return id
}

func ladderGoldenView(t *testing.T, c Card) map[string]any {
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
	for _, key := range []string{"action_type", "status", "ladder"} {
		out[key] = all[key]
	}
	var why []any
	for _, r := range all["why_approval"].([]any) {
		if r.(map[string]any)["code"] == "not_rehearsed" {
			why = append(why, r)
		}
	}
	out["why_approval"] = why
	return out
}

func TestLadder_LoaderReadsTheQueueRow(t *testing.T) {
	pool, ctx := livePool(t)
	queueID := queueLadderDrop(t, ctx, pool)
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "autonomous"}
	c, err := l.Card(ctx, queueID)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if c.Ladder != nil || c.LadderNote != LadderNoneRecorded {
		t.Fatalf("NULL ladder: %+v note %q, want the none-recorded note", c.Ladder,
			c.LadderNote)
	}
	if err := verify.AppendLadder(ctx, pool, queueID, tierOne(), cloneSkipped()); err != nil {
		t.Fatalf("append: %v", err)
	}
	if c, err = l.Card(ctx, queueID); err != nil {
		t.Fatalf("card: %v", err)
	}
	if len(c.Ladder) != 2 || c.LadderNote != "" || !hasReason(c, "not_rehearsed") {
		t.Fatalf("ladder %+v note %q why %+v", c.Ladder, c.LadderNote, c.Why)
	}
	got := ladderGoldenView(t, c)
	if os.Getenv("SAGE_UPDATE_GOLDEN") == "1" {
		raw, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(ladderCardGolden, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	raw, err := os.ReadFile(ladderCardGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("golden: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("card JSON differs from %s:\n%s", ladderCardGolden, g)
	}
}

func TestLadder_LoaderToleratesANonArrayLadder(t *testing.T) {
	pool, ctx := livePool(t)
	queueID := queueLadderDrop(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET ladder = '{"tier":2}'::jsonb
		WHERE id = $1`, queueID); err != nil {
		t.Fatal(err)
	}
	buf := captureWarnings(t)
	c, err := Loader{Pool: pool, Database: "orders"}.Card(ctx, queueID)
	if err != nil {
		t.Fatalf("card: %v (a bad ladder must not fail the card)", err)
	}
	if c.Ladder != nil || c.LadderNote != "" || c.QueueID != queueID {
		t.Fatalf("ladder %+v note %q", c.Ladder, c.LadderNote)
	}
	if want := fmt.Sprintf("queue_id=%d", queueID); !strings.Contains(buf.String(), want) {
		t.Fatalf("warnings %q do not name %s", buf.String(), want)
	}
}
