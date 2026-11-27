package executor

import (
	"strings"
	"testing"
	"time"
)

// executor-01 review: the other DB tests build the restore config with
// findingMonitorConfig themselves, so they pass even if monitorFinding (the
// production entry after an autonomous drop) still wires the standing
// authorizer. This one starts the monitor the way ApplyFinding does.
func TestDropRestoreThroughMonitorFinding(t *testing.T) {
	f := newRestoreFixture(t, false)
	_ = f.e.SetTrustLevel("observation") // only the restore path passes this

	f.e.monitorFinding(f.ctx, f.finding, f.action)
	waitMonitors(t, f.e, 2*time.Minute)

	if outcome, reason := f.outcome(t); outcome != "rolled_back" {
		t.Fatalf("outcome = %q (%s), want rolled_back by the monitor", outcome, reason)
	}
	var valid bool
	var def string
	if err := f.pool.QueryRow(f.ctx, `SELECT i.indisvalid, pg_get_indexdef(i.indexrelid)
		FROM pg_index i WHERE i.indexrelid = to_regclass($1)`, "public."+f.index).
		Scan(&valid, &def); err != nil || !valid {
		t.Fatalf("restored index valid = %v (%v), want a valid index", valid, err)
	}
	if !strings.Contains(def, "(a)") {
		t.Fatalf("restored definition = %q, want the dropped index on (a)", def)
	}
	verdict, reason, gate := f.restoreDecision(t)
	if verdict != "execute" || gate != "drop_restore_bypass" ||
		!strings.Contains(reason, "trust observation") {
		t.Fatalf("restore decision = %s/%s (%s), want the restore bypass", verdict, gate, reason)
	}
}

// waitMonitors waits for every monitor e started, or fails after limit.
func waitMonitors(t *testing.T, e *Executor, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		e.monitors.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("monitor still running after %s", limit)
	}
}
