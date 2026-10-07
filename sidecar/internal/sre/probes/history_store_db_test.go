package probes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. plan_regressions reads query_store, so it runs on
// the history store (scoped to this database); every other probe keeps
// reading the monitored database's catalog.
func TestPlanRegressionsIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	got := map[histstore.Mode]string{}
	now := time.Now() // the same fixture (absolute times) in both placements
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		st := histstore.Resolve(p.Monitored)
		for _, s := range []sample{{30 * time.Minute, 0, 0, "v1:a"},
			{20 * time.Minute, 10, 5, "v1:a"}, {10 * time.Minute, 20, 55, "v1:b"}} {
			if _, err := st.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid,
				calls, total_exec_time, mean_exec_time, plan_hash{dbcol})
				VALUES ($1, 881001, $2, $3, 0, $4{dbval})`,
				now.Add(-s.ago), s.calls, s.total, s.hash); err != nil {
				t.Fatal(err)
			}
		}
		// Another database flips the same query id the other way, later.
		p.Noise(t, time.Now().Add(-5*time.Minute), 881001)
		p.Noise(t, time.Now().Add(-2*time.Minute), 881001)
		res := NewRunner(p.Monitored, Catalog(), NewLimiter(1)).Run(ctx, PlanRegressions,
			Args{})
		shifts, err := PlanShifts(res)
		if err != nil {
			t.Fatalf("%s plan_regressions: %+v (%v)", mode, res, err)
		}
		if len(shifts) != 1 {
			t.Fatalf("%s: %d shifts, want this database's one: %+v", mode, len(shifts), shifts)
		}
		got[mode] = fmt.Sprintf("%+v", shifts[0])
	}
	if got[histstore.ModeMonitored] != got[histstore.ModeMeta] {
		t.Fatalf("plan shifts differ:\nmonitored %s\nmeta      %s",
			got[histstore.ModeMonitored], got[histstore.ModeMeta])
	}
}

// Exactly the probes that read pg_sage's history run on the history store;
// every other probe reads the monitored database's catalog.
func TestHistoryProbesAreExactlyTheHistoryReaders(t *testing.T) {
	reg := Catalog()
	marked := 0
	for id, spec := range reg.specs {
		reads := false
		for _, v := range spec.Variants {
			reads = reads || strings.Contains(v.SQL, "sage.query_store") ||
				strings.Contains(v.SQL, "sage.snapshots")
		}
		if reads != spec.History {
			t.Fatalf("probe %s: reads history %v, marked history %v", id, reads, spec.History)
		}
		if spec.History {
			marked++
		}
	}
	if marked == 0 || !reg.specs[PlanRegressions].History {
		t.Fatal("plan_regressions must be a history probe")
	}
}
