package runway

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-02 review on real PostgreSQL: unreadable replication slots do
// not clear a slot finding whose series has no sample.
//
// No concurrency test: one monitor goroutine ticks per runtime.

// openSlotFinding opens a warning WAL forecast finding for slot name.
func openSlotFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) int64 {
	t.Helper()
	ident := "slot:" + name
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings
			WHERE category = $1 AND object_identifier = $2`, CategoryWAL, ident)
	})
	r := Runway{Category: CategoryWAL, ObjectType: "replication_slot", Identifier: ident,
		Kind: sre.TriggerDiskWAL, Subject: "slot " + name, Severity: SeverityWarning,
		Title:          "WAL retained by slot " + name + " reaches its maximum in about 2d",
		Recommendation: "test", Detail: map[string]any{"runway_kind": "wal_slot"}}
	if _, err := analyzer.UpsertFindingsWithResult(ctx, pool,
		[]analyzer.Finding{r.Finding()}); err != nil {
		t.Fatalf("open slot finding: %v", err)
	}
	id, _, ok := openFinding(t, ctx, pool, CategoryWAL, ident)
	if !ok {
		t.Fatalf("slot finding %s did not open", ident)
	}
	return id
}

// Unreadable slots hold the open finding of a slot with no samples (it
// may still exist); once the slots are readable and the slot is gone, the
// finding resolves.
func TestMonitorTick_UnreadableSlotsHoldASlotFindingWithoutSamples(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("held_%d", time.Now().UnixNano())
	id := openSlotFinding(t, ctx, pool, name)
	run := &probeFailer{inner: liveRunner(pool),
		fail: map[probes.ID]bool{probes.ReplicationSlots: true}}
	m := newTestMonitor(t, pool, run, nil)
	res, err := m.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), string(probes.ReplicationSlots)) {
		t.Fatalf("tick error = %v, want the unreadable slots named", err)
	}
	if !hasCategory(res.Resolved, CategoryWAL) {
		t.Fatalf("WAL not evaluated (resolved %v); the hold would prove nothing",
			res.Resolved)
	}
	if got, _, ok := openFinding(t, ctx, pool, CategoryWAL, "slot:"+name); !ok ||
		got != id {
		t.Fatalf("slot finding open=%v id %d, want %d held open", ok, got, id)
	}
	if stale, reason, _ := staleDetail(t, ctx, pool, CategoryWAL,
		"slot:"+name); !stale || reason != HoldNoSamples {
		t.Fatalf("slot finding stale=%v reason %q, want %q", stale, reason, HoldNoSamples)
	}
	delete(run.fail, probes.ReplicationSlots)
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("readable tick: %v", err)
	}
	if _, _, ok := openFinding(t, ctx, pool, CategoryWAL, "slot:"+name); ok {
		t.Fatal("the slot is gone from a readable reading, yet its finding stayed open")
	}
}
