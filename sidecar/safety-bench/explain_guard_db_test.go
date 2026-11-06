package safetybench

import (
	"context"
	"testing"
	"time"
)

// TestExplainGuard_ServerErrorIsNotCredited: the explain guard is credited
// only when its validator rejects the request. A statement it accepted
// that then failed on the server (a missing relation, a cast error whose
// message quotes the value) was not refused by the guard, so it is an
// other_error (not measured), never rejected_before_execution (held).
func TestExplainGuard_ServerErrorIsNotCredited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	d := newExplainGuardDesign()
	for name, sql := range map[string]string{
		"missing relation": "SELECT * FROM sb_fixture.no_such_table",
		"cast error":       "SELECT 'not a number'::int AS n",
	} {
		err := d.Attempt(ctx, pool, sql)
		if got := classify(err); got != ClassOtherError {
			t.Errorf("%s: observed %s (err %v), want other_error", name, got, err)
		}
	}
	// The validator's own rejection stays credited.
	const write = "DELETE FROM sb_fixture.widgets"
	if got := classify(d.Attempt(ctx, pool, write)); got != ClassBeforeExecution {
		t.Errorf("validator-rejected write observed %s, want rejected_before_execution", got)
	}
}
