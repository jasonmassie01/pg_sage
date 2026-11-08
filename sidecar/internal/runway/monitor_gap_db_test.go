package runway

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// forecast-02: a gap longer than the lookback neither resolves nor
// reopens the forecast finding. The xid_runway reading has no counters,
// so only the seeded xid samples exist (a live sample would start a new
// epoch), and an age past the freeze maximum, so the trend is projected
// (forecast-01: without a reading it would be held, not projected).

func xidTriggerKeys(st *fakeStarter) []string {
	var keys []string
	for _, tr := range st.started() {
		if tr.Subject == "xid" {
			keys = append(keys, tr.IdempotencyKey)
		}
	}
	return keys
}

func countFindings(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cat,
	ident string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE category = $1 AND object_identifier = $2`, cat, ident).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	return n
}

// CHECK-02/03: open, then a 7 h gap and two ticks, then measured again:
// one finding row throughout, held stale across the gap and refreshed
// after it, and one idempotency key (one finding id) for the object.
func TestMonitorTick_GapKeepsOneFindingAndOneKey(t *testing.T) {
	pool, ctx := livePool(t)
	resetXID(t, ctx, pool)
	seedXIDTrend(t, ctx, pool, 0)
	st := &fakeStarter{}
	m := newTestMonitor(t, pool, pastGateXID(liveRunner(pool)), st)
	mustTick(t, ctx, m)
	id, sev, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid")
	if !ok || sev != SeverityCritical {
		t.Fatalf("measured xid trend: finding open=%v severity %q", ok, sev)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.runway_samples
		SET sampled_at = sampled_at - interval '7 hours' WHERE kind = 'xid'`); err != nil {
		t.Fatalf("age the samples: %v", err)
	}
	for i := 0; i < 2; i++ {
		mustTick(t, ctx, m)
		got, _, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid")
		if !ok || got != id {
			t.Fatalf("gap tick %d: finding open=%v id %d, want %d", i+1, ok, got, id)
		}
		if stale, reason, _ := staleDetail(t, ctx, pool, CategoryWraparound,
			"xid"); !stale || reason != HoldNoSamples {
			t.Fatalf("gap tick %d: stale=%v reason %q, want %q", i+1, stale, reason,
				HoldNoSamples)
		}
	}
	seedXIDTrend(t, ctx, pool, 0)
	mustTick(t, ctx, m)
	if got, _, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid"); !ok || got != id {
		t.Fatalf("measured again: finding open=%v id %d, want %d", ok, got, id)
	}
	if stale, _, _ := staleDetail(t, ctx, pool, CategoryWraparound, "xid"); stale {
		t.Fatal("a measured upsert kept the stale mark")
	}
	if n := countFindings(t, ctx, pool, CategoryWraparound, "xid"); n != 1 {
		t.Fatalf("%d xid finding rows across the gap, want 1 (no resolve/reopen)", n)
	}
	want := fmt.Sprintf("runway:%d:%s", id, SeverityCritical)
	keys := xidTriggerKeys(st)
	if len(keys) == 0 {
		t.Fatal("no xid investigation was started")
	}
	for _, k := range keys {
		if k != want {
			t.Fatalf("idempotency keys %v, want only %s", keys, want)
		}
	}
}

// refuseStale makes sage.findings reject any update that marks a finding
// stale, so markStale fails while resolving still works.
func refuseStale(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION public.sage_test_refuse_stale() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
		    IF NEW.detail ? 'stale' THEN
		        RAISE EXCEPTION 'refused stale mark (test)';
		    END IF;
		    RETURN NEW;
		END $$;
		DROP TRIGGER IF EXISTS sage_test_refuse_stale ON sage.findings;
		CREATE TRIGGER sage_test_refuse_stale BEFORE UPDATE ON sage.findings
		FOR EACH ROW EXECUTE FUNCTION public.sage_test_refuse_stale()`); err != nil {
		t.Fatalf("install the refusing trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS sage_test_refuse_stale ON sage.findings;
			DROP FUNCTION IF EXISTS public.sage_test_refuse_stale()`)
	})
}

// Error propagation: a failing markStale is joined into the tick's error
// and the other categories still resolve.
func TestMonitorTick_StaleMarkFailureIsJoined(t *testing.T) {
	pool, ctx := livePool(t)
	resetXID(t, ctx, pool)
	id := openXIDFinding(t, ctx, pool)
	gone := fmt.Sprintf("public.gone_%d_seq", time.Now().UnixNano())
	seq := Runway{Category: CategorySequence, ObjectType: "sequence", Identifier: gone,
		Severity: SeverityWarning, Title: "Sequence " + gone + " reaches its limit",
		Recommendation: "test", Detail: map[string]any{"runway_kind": "sequence"}}
	if _, err := analyzer.UpsertFindingsWithResult(ctx, pool,
		[]analyzer.Finding{seq.Finding()}); err != nil {
		t.Fatalf("open sequence finding: %v", err)
	}
	refuseStale(t, ctx, pool)
	m := newTestMonitor(t, pool, liveRunner(pool), nil)
	res, err := m.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "mark held runway findings stale") ||
		!strings.Contains(err.Error(), "refused stale mark") {
		t.Fatalf("tick error = %v, want the failed stale mark joined", err)
	}
	if _, _, ok := openFinding(t, ctx, pool, CategorySequence, gone); ok {
		t.Fatalf("the dropped sequence's finding stayed open (resolved %v)", res.Resolved)
	}
	if got, _, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid"); !ok ||
		got != id {
		t.Fatalf("held xid finding open=%v id %d, want %d", ok, got, id)
	}
}
