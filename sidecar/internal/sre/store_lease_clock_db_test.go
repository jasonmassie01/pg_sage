package sre

import (
	"errors"
	"fmt"
	"testing"
)

// Model a database wall clock that moved backward after a lease started.
// Changing only this owned row makes the regression deterministic without
// changing the server clock or sleeping through a lease.
func TestStore_BackwardClockNeverRefundsActiveTime(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	for _, method := range []string{"conclude", "release", "stop"} {
		for _, charged := range []int64{0, 20000} {
			t.Run(fmt.Sprintf("%s/charged=%d", method, charged), func(t *testing.T) {
				scope := testScope(t, ctx, st)
				lease, ev := evaluating(t, ctx, st, scope, "backward-clock")
				if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
					SET active_ms=$6, lease_started_at=statement_timestamp()+interval '10 seconds',
					    lease_until=statement_timestamp()+interval '30 seconds',
					    segment_deadline=statement_timestamp()+interval '60 seconds'
					WHERE deployment_id=$1 AND database_id=$2 AND id=$3
					  AND lease_owner=$4 AND fence_token=$5`, append(leaseArgs(lease), charged)...); err != nil {
					t.Fatal(err)
				}
				var got Investigation
				var err error
				switch method {
				case "conclude":
					got, err = st.Conclude(ctx, lease, concluded(ev))
				case "release":
					got, err = st.Release(ctx, lease, StateNeedsEvidence)
				case "stop":
					var current Investigation
					current, err = st.Get(ctx, scope, lease.InvestigationID)
					if err == nil {
						got, err = st.Stop(ctx, scope, lease.InvestigationID, current.Version)
					}
				}
				if err != nil || got.ActiveMS != charged || got.LeaseOwner != "" {
					t.Fatalf("backward clock refunded prior charge or broke completion: active=%d want=%d owner=%s err=%v", got.ActiveMS, charged, got.LeaseOwner, err)
				}
				if _, err := st.CommitStep(ctx, lease, step("stale-clock-worker", StateCollecting)); !errors.Is(err, ErrLeaseLost) {
					t.Fatalf("completed lease accepted a stale worker: %v", err)
				}
				if err := st.VerifyEvents(ctx, scope, lease.InvestigationID); err != nil {
					t.Fatalf("completion lost its durable event chain: %v", err)
				}
			})
		}
	}
}

func TestStore_BackwardClockOrphanCannotRefundActiveTime(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	for _, charged := range []int64{0, 20000} {
		t.Run(fmt.Sprintf("charged=%d", charged), func(t *testing.T) {
			scope := testScope(t, ctx, st)
			inv, _, err := st.Create(ctx, lockStart(scope, "backward-clock-orphan"))
			if err != nil {
				t.Fatal(err)
			}
			old, err := st.Claim(ctx, scope, inv.ID, NewUUID())
			if err != nil {
				t.Fatal(err)
			}
			// An inverted persisted interval can result if the clock changes
			// between timestamp evaluations. Reclaiming may not give time back.
			if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
				SET active_ms=$6, lease_started_at=statement_timestamp()+interval '10 seconds',
				    lease_until=statement_timestamp()-interval '1 second',
				    segment_deadline=statement_timestamp()+interval '60 seconds'
				WHERE deployment_id=$1 AND database_id=$2 AND id=$3
				  AND lease_owner=$4 AND fence_token=$5`, append(leaseArgs(old), charged)...); err != nil {
				t.Fatal(err)
			}
			next, err := st.Claim(ctx, scope, inv.ID, NewUUID())
			if err != nil {
				t.Fatalf("negative orphan interval broke reclaim: %v", err)
			}
			got, err := st.Get(ctx, scope, inv.ID)
			if err != nil || got.ActiveMS != charged || next.Fence != old.Fence+1 {
				t.Fatalf("reclaim refunded prior charge or lost fence: active=%d want=%d fence=%d err=%v", got.ActiveMS, charged, next.Fence, err)
			}
			if _, err := st.Heartbeat(ctx, old); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("orphaned worker heartbeat survived reclaim: %v", err)
			}
			if _, err := st.CommitStep(ctx, next, step("new-clock-worker", StateEvaluating)); err != nil {
				t.Fatalf("new fenced worker could not commit: %v", err)
			}
		})
	}
}
