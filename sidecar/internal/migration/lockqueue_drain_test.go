package migration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/rca"
)

// State transitions: a ddl_lock_queue finding (a DDL the advisor scored
// low) follows the live queue like any other: it is re-upserted on every
// poll, and when the queue drains it drops back to warning without the
// escalation, so the case loses the cancel candidate instead of staying
// critical until the 24 h stale resolver.
func TestLockQueue_QuietAdvisorFindingFollowsTheQueue(t *testing.T) {
	quiet := func() *rca.Incident { return nil }
	src := &fakeQueueSource{fresh: true, at: time.Now()}
	ddl := waitingDDL(200)
	ddl.Query = "ALTER TABLE public.orders ADD COLUMN note text"
	ddls := []activeDDL{ddl}
	d, sink, analyses := queueDetector(&ddls, quiet, src)
	var severities []string
	for _, waiters := range []int{3, 0, 5} {
		src.chains = []analyzer.LockChain{queueChain(200, waiters)}
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		f := sink.last(t)
		severities = append(severities, f.Severity)
		_, escalated := f.Detail["lock_queue_escalated"]
		if escalated != (waiters >= 3) {
			t.Fatalf("%d waiters: escalated=%v, detail %v", waiters, escalated, f.Detail)
		}
		if f.Detail["lock_queue_waiters"] != waiters || f.RuleID != lockQueueRule {
			t.Fatalf("%d waiters: finding %+v", waiters, f)
		}
	}
	if got := strings.Join(severities, ","); got != "critical,warning,critical" {
		t.Fatalf("severities over the queue = %s", got)
	}
	if *analyses != 1 || sink.count() != 3 {
		t.Fatalf("analyses=%d upserts=%d, want 1 analysis and 3 upserts", *analyses,
			sink.count())
	}
	if _, ok := sink.findings[1].Detail["cancel_guidance_sql"]; ok {
		t.Fatalf("drained queue kept cancel guidance: %v", sink.findings[1].Detail)
	}
}

// Nil/empty: a quiet DDL with no queue ever observed opens no finding.
func TestLockQueue_QuietAdvisorNoQueueNoFinding(t *testing.T) {
	quiet := func() *rca.Incident { return nil }
	ddls := []activeDDL{waitingDDL(200)}
	d, sink, _ := queueDetector(&ddls, quiet, &fakeQueueSource{})
	for i := 0; i < 2; i++ {
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	if sink.count() != 0 {
		t.Fatalf("quiet DDL with no queue persisted %d findings", sink.count())
	}
}
