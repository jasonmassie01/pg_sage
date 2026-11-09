package optimizer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-03: PartitionPlan is the analyzer's entry to the optimizer's
// partitioned-table build plan.
//
// No concurrent access tests: PartitionPlan is a pure function.

func planSnap(nested bool) *collector.Snapshot {
	snap := &collector.Snapshot{Partitions: []collector.PartitionInfo{
		{ChildSchema: "s", ChildTable: "t_2", ParentSchema: "s", ParentTable: "t"},
		{ChildSchema: "s", ChildTable: "t_1", ParentSchema: "s", ParentTable: "t"},
		{ChildSchema: "s", ChildTable: "u_1", ParentSchema: "s", ParentTable: "u"},
	}}
	if nested {
		snap.Partitions = append(snap.Partitions, collector.PartitionInfo{
			ChildSchema: "s", ChildTable: "t_1a", ParentSchema: "s", ParentTable: "t_1"})
	}
	return snap
}

func TestPartitionPlan_FromSnapshot(t *testing.T) {
	spec := IndexSpec{Name: "t_k_idx", Method: "btree", Keys: `"k"`}
	plan := PartitionPlan(planSnap(false), "s", "t", spec)
	if len(plan) != 5 {
		t.Fatalf("plan = %q, want ON ONLY + 2 statements for each of 2 partitions", plan)
	}
	if plan[0] != `CREATE INDEX IF NOT EXISTS "t_k_idx" ON ONLY "s"."t" USING btree ("k")` {
		t.Errorf("plan[0] = %q", plan[0])
	}
	// Partitions in name order; another parent's partitions are left out.
	if !strings.Contains(plan[1], `ON "s"."t_1" `) || !strings.Contains(plan[3], `ON "s"."t_2" `) {
		t.Errorf("partition steps = %q, %q, want t_1 then t_2", plan[1], plan[3])
	}
	for _, step := range plan {
		if strings.Contains(step, "u_1") {
			t.Errorf("step %q builds on another parent's partition", step)
		}
	}
}

func TestPartitionPlan_NoPlanWithoutFlatPartitions(t *testing.T) {
	spec := IndexSpec{Name: "t_k_idx", Method: "btree", Keys: `"k"`}
	if plan := PartitionPlan(nil, "s", "t", spec); plan != nil {
		t.Errorf("nil snapshot plan = %q, want nil", plan)
	}
	if plan := PartitionPlan(planSnap(false), "s", "none", spec); plan != nil {
		t.Errorf("table without partitions plan = %q, want nil", plan)
	}
	if plan := PartitionPlan(planSnap(true), "s", "t", spec); plan != nil {
		t.Errorf("sub-partitioned plan = %q, want nil", plan)
	}
}
