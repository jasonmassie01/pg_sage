package executor

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// plan-02a: AnyInFlight reports a change in flight or in verification on
// the database whatever its objects (a plan's setting step waits for it).
func TestVerificationWaits_AnyInFlightSeesEveryWatchedChange(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	w := waitExecutor(t, pool).VerificationWaits()
	if busy, err := w.AnyInFlight(ctx); err != nil || busy {
		t.Fatalf("empty database: busy %v (%v), want false", busy, err)
	}
	id := recordInFlight(t, pool, ctx, "CREATE INDEX CONCURRENTLY i ON public.t (a)",
		"DROP INDEX CONCURRENTLY public.i", "monitoring", time.Minute)
	if busy, err := w.AnyInFlight(ctx); err != nil || !busy {
		t.Fatalf("a watched index: busy %v (%v), want true", busy, err)
	}
	mustExec(t, pool, ctx, `UPDATE sage.action_log SET outcome = 'success' WHERE id = `+
		strconv.FormatInt(id, 10))
	if busy, err := w.AnyInFlight(ctx); err != nil || busy {
		t.Fatalf("a concluded change: busy %v (%v), want false", busy, err)
	}
}

// Nil and poolless waits fail closed with an error, never "nothing in flight".
func TestVerificationWaits_AnyInFlightWithoutAPoolIsAnError(t *testing.T) {
	var nilWaits *VerificationWaits
	if _, err := nilWaits.AnyInFlight(context.Background()); err == nil {
		t.Fatal("nil waits: want an error")
	}
	if _, err := (&VerificationWaits{}).AnyInFlight(context.Background()); err == nil {
		t.Fatal("poolless waits: want an error")
	}
}
