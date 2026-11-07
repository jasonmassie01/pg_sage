package tuning

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// indexes-03: tuning's covered_by drops carry the same standby gate as the
// unused_index rule. Primary statistics miss replica reads, so a drop on a
// database with standbys says so and is never automatic.
// No concurrent access tests: a validator lives for one cycle and one
// goroutine.

func judgeDropOn(t *testing.T, cur *collector.Snapshot, index string) Judged {
	t.Helper()
	h := newHarness(t)
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0), nil, nil)
	return v.judge(context.Background(), ordersCase(), ordersEvidence(), dropProposal(index))
}

func streamingReplicas(cur *collector.Snapshot, n int) {
	cur.Replication = &collector.ReplicationStats{}
	for i := 0; i < n; i++ {
		cur.Replication.Replicas = append(cur.Replication.Replicas,
			collector.ReplicaInfo{State: "streaming"})
	}
}

func TestJudge_IndexDropCoveredIsStillAdmitted(t *testing.T) {
	j := judgeDropOn(t, validationSnap(), "public.orders_customer_idx")
	if j.Verdict != VerdictAdmitted || j.Finding == nil {
		t.Fatalf("(customer_id) covered by (customer_id, status): %+v", j)
	}
	f := *j.Finding
	if f.Detail["covered_by"] != "orders_customer_status_idx" ||
		f.ActionRisk != optimizer.RiskHigh || f.Detail["action_risk"] != optimizer.RiskHigh {
		t.Fatalf("finding = %+v", f)
	}
	if _, has := f.Detail["unused_since"]; has {
		t.Fatalf("a covered_by drop has no unused evidence: %v", f.Detail)
	}
	if !strings.Contains(f.Recommendation, "is covered by orders_customer_status_idx") {
		t.Fatalf("recommendation = %q", f.Recommendation)
	}
}

func TestJudge_IndexDropOnAStreamingReplicaNeedsApproval(t *testing.T) {
	cur := validationSnap()
	streamingReplicas(cur, 1)
	j := judgeDropOn(t, cur, "public.orders_customer_idx")
	if j.Verdict != VerdictAdmitted || j.Finding == nil {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	reason, _ := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(reason, "1 streaming replica(s)") ||
		f.Detail["replica_index_usage"] != "unknown" || f.Detail["streaming_replicas"] != 1 {
		t.Fatalf("the standby gate is applied to the tuning drop: %v", f.Detail)
	}
	if !strings.Contains(f.Recommendation, "Not dropped automatically") ||
		f.Detail["covered_by"] != "orders_customer_status_idx" ||
		f.ActionRisk != optimizer.RiskHigh {
		t.Fatalf("finding = %+v", f)
	}
}

func TestJudge_IndexDropWithoutReplicasHasNoStandbyGate(t *testing.T) {
	for name, mutate := range map[string]func(*collector.Snapshot){
		"nil replication":   func(*collector.Snapshot) {},
		"zero replicas":     func(s *collector.Snapshot) { streamingReplicas(s, 0) },
		"logical slot only": logicalSlotOnly,
	} {
		t.Run(name, func(t *testing.T) {
			cur := validationSnap()
			mutate(cur)
			j := judgeDropOn(t, cur, "public.orders_customer_idx")
			if j.Verdict != VerdictAdmitted || j.Finding == nil {
				t.Fatalf("judged = %+v", j)
			}
			for _, k := range []string{analyzer.DetailApprovalRequired,
				"replica_index_usage", "streaming_replicas", "physical_slots"} {
				if _, has := j.Finding.Detail[k]; has {
					t.Fatalf("%s set without replicas: %v", k, j.Finding.Detail)
				}
			}
		})
	}
}

func logicalSlotOnly(s *collector.Snapshot) {
	s.Replication = &collector.ReplicationStats{Slots: []collector.SlotInfo{
		{SlotName: "cdc", SlotType: "logical"}}}
}

// Replication state that could not be read is unknown, never "no
// replicas": the drop still needs approval.
func TestJudge_IndexDropWithUnreadableReplicationIsUnknown(t *testing.T) {
	cur := validationSnap()
	cur.Unavailable = map[string]string{"replication": "permission denied"}
	j := judgeDropOn(t, cur, "public.orders_customer_idx")
	if j.Verdict != VerdictAdmitted || j.Finding == nil {
		t.Fatalf("judged = %+v", j)
	}
	reason, _ := j.Finding.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(reason, "could not be read") ||
		j.Finding.Detail["replica_index_usage"] != "unknown" {
		t.Fatalf("detail = %v", j.Finding.Detail)
	}
}

// A refused drop never reaches the gate: there is no finding to annotate.
func TestJudge_RefusedDropOnAReplicaHasNoFinding(t *testing.T) {
	cur := validationSnap()
	streamingReplicas(cur, 2)
	j := judgeDropOn(t, cur, "public.orders_status_idx")
	if j.Verdict != VerdictRejected || j.Finding != nil {
		t.Fatalf("judged = %+v", j)
	}
}
