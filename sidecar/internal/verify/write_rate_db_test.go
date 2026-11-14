package verify

import (
	"strings"
	"testing"
	"time"
)

// executor-03b review, on real PostgreSQL: WAL per written tuple depends on
// the write rate. After each checkpoint the first change to a page writes
// a full-page image, so a table that changes the same pages with fewer
// tuples pays more per tuple. Here the updates are HOT (fillfactor 50, the
// indexed column untouched), so the new index costs the writes nothing;
// only the rate falls tenfold after the build. The per-tuple WAL rises far
// over 20%, and the signal must call that insufficient, not write_impact.
func TestSlowerWritesAfterTheBuildAreNotChargedToTheIndex(t *testing.T) {
	pool := verifyExclusivePool(t, "WAL per tuple across forced CHECKPOINTs")
	ctx := t.Context()
	executedAt := time.Date(2098, 3, 9, 12, 0, 0, 0, time.UTC)
	f := newWriteFixture(t, ctx, pool, executedAt, "verify_wsig_rate")
	f.write("verify_wsig_rate", 100000, `CREATE TABLE public.verify_wsig_rate
		WITH (fillfactor = 50, autovacuum_enabled = false) AS
		SELECT g AS id, g % 97 AS customer, 0 AS touched FROM generate_series(1, 100000) g`)
	// update writes every modulo-th row once: the rows are spread over
	// every page of the table, whatever the count.
	update := func(modulo, k int) {
		f.exec("CHECKPOINT")
		f.write("verify_wsig_rate", int64(100000/modulo), `UPDATE public.verify_wsig_rate
			SET touched = touched + 1 WHERE id % $1 = $2`, modulo, k)
	}
	f.cycle(executedAt.Add(-55 * time.Minute))
	for i := 0; i < 5; i++ {
		update(5, i)
		f.cycle(executedAt.Add(time.Duration(-45+10*i) * time.Minute))
	}
	f.exec("CREATE INDEX idx_verify_wsig_rate ON public.verify_wsig_rate(customer)")
	f.cycle(executedAt.Add(5 * time.Minute))
	for i := 0; i < 5; i++ {
		update(50, i)
		f.cycle(executedAt.Add(time.Duration(15+10*i) * time.Minute))
	}
	seedQueryGain(t, ctx, pool, executedAt)
	verdict, actionID := watchWriteFixture(t, ctx, pool, executedAt,
		"public.verify_wsig_rate", "public.idx_verify_wsig_rate")
	signal := writeSignalOf(t, verdict)
	t.Logf("write_signal = %v", signal)
	before, after := signalSide(t, signal, "before"), signalSide(t, signal, "after")
	beforeCost, _ := before["bytes_per_tuple"].(float64)
	afterCost, _ := after["bytes_per_tuple"].(float64)
	if before["intervals"] != 5 || after["intervals"] != 5 || afterCost <= 1.2*beforeCost {
		t.Fatalf("sides = %#v / %#v, want 5 measured intervals each and the per-tuple "+
			"WAL rising over 20%% (the scenario this test guards)", before, after)
	}
	if !verdict.Retain || verdict.Revert || signal["outcome"] != writeInsufficient ||
		signal["decisive"] != false {
		t.Fatalf("verdict = %#v (signal %#v), want retained with the signal insufficient",
			verdict, signal)
	}
	if reason, _ := signal["insufficient"].(string); !strings.Contains(reason, "tuples/s") {
		t.Fatalf("write_signal = %#v, want a reason naming the write rates", signal)
	}
	assertStoredVerdict(t, ctx, pool, actionID, "success")
}
