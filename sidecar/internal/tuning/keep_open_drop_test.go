package tuning

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// indexes-03 binds the tuning drops already open, not only new proposals.
// keepOpen re-emits an open finding every cycle, so a never-scanned drop
// stored before the upgrade (its false "statistics since" evidence, no
// standby gate) would stay on its approval card for good, and a covered_by
// drop stored before a standby was attached would stay automatic.
// No concurrent access tests: one agent cycle runs under the agent's mutex.

func openDrop(detail map[string]any) analyzer.Finding {
	return analyzer.Finding{Category: CategoryIndexDrop, Severity: "info",
		ObjectType: "index", ObjectIdentifier: "public.orders_status_idx",
		Title: "Drop index public.orders_status_idx on public.orders", Detail: detail,
		Recommendation: "redundant (public.orders_status_idx is covered by orders_sc_idx)",
		RecommendedSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.orders_status_idx",
		ActionRisk:     "high_risk"}
}

func coveredDropDetail() map[string]any {
	return map[string]any{"producer": Producer, "case_id": "write_amplification:x",
		"table": "public.orders", "index": "public.orders_status_idx",
		"covered_by": "orders_sc_idx"}
}

// tuneOn runs one cycle on ordersPair with cur adjusted by mutate.
func tuneOn(t *testing.T, h *harness, mutate func(*collector.Snapshot)) []analyzer.Finding {
	t.Helper()
	prev, cur := ordersPair()
	mutate(cur)
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	return out.Findings
}

func TestTune_OpenNeverScannedDropResolves(t *testing.T) {
	for name, detail := range map[string]map[string]any{
		"unused_since evidence": {"producer": Producer, "case_id": "write_amplification:x",
			"table": "public.orders", "index": "public.orders_status_idx",
			"unused_since": "2026-10-01T00:00:00Z"},
		"nil detail": nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.store.open = []analyzer.Finding{openDrop(detail)}
			fs := tuneOn(t, h, func(*collector.Snapshot) {})
			if f, ok := byIdent(fs, "public.orders_status_idx"); ok {
				t.Fatalf("a drop no other index covers is re-emitted: %+v", f)
			}
			if !h.logs.contains("unused_index rule owns") {
				t.Fatal("the resolution names the unused_index rule")
			}
		})
	}
}

func TestTune_OpenCoveredDropGetsTheStandbyGate(t *testing.T) {
	h := newHarness(t)
	open := openDrop(coveredDropDetail())
	h.store.open = []analyzer.Finding{open}
	var got analyzer.Finding
	for cycle := 0; cycle < 2; cycle++ {
		fs := tuneOn(t, h, func(s *collector.Snapshot) { streamingReplicas(s, 1) })
		var ok bool
		if got, ok = byIdent(fs, open.ObjectIdentifier); !ok {
			t.Fatalf("cycle %d: the covered drop is kept open", cycle)
		}
		reason, _ := got.Detail[analyzer.DetailApprovalRequired].(string)
		if !strings.Contains(reason, "1 streaming replica(s)") ||
			got.Detail["replica_index_usage"] != "unknown" ||
			got.Detail["covered_by"] != "orders_sc_idx" {
			t.Fatalf("cycle %d: detail = %v", cycle, got.Detail)
		}
		h.store.open = []analyzer.Finding{got} // the analyzer stores what was emitted
	}
	if n := strings.Count(got.Recommendation, "Not dropped automatically"); n != 1 {
		t.Fatalf("the gate is applied once, not every cycle (%d): %q", n,
			got.Recommendation)
	}
	if _, has := open.Detail[analyzer.DetailApprovalRequired]; has {
		t.Fatal("the stored open finding's detail map was mutated")
	}
}

func TestTune_OpenCoveredDropWithUnreadableReplicationIsGated(t *testing.T) {
	h := newHarness(t)
	h.store.open = []analyzer.Finding{openDrop(coveredDropDetail())}
	fs := tuneOn(t, h, func(s *collector.Snapshot) {
		s.Unavailable = map[string]string{"replication": "permission denied"}
	})
	got, ok := byIdent(fs, "public.orders_status_idx")
	if !ok {
		t.Fatal("the covered drop is kept open")
	}
	if reason, _ := got.Detail[analyzer.DetailApprovalRequired].(string); !strings.Contains(
		reason, "could not be read") {
		t.Fatalf("unreadable replication gates as unknown: %v", got.Detail)
	}
}

func TestTune_OpenCoveredDropWithoutReplicasIsUnchanged(t *testing.T) {
	h := newHarness(t)
	open := openDrop(coveredDropDetail())
	h.store.open = []analyzer.Finding{open}
	fs := tuneOn(t, h, func(s *collector.Snapshot) { streamingReplicas(s, 0) })
	got, ok := byIdent(fs, open.ObjectIdentifier)
	if !ok || !reflect.DeepEqual(got, open) {
		t.Fatalf("without standbys the open drop is re-emitted unchanged: %+v", got)
	}
}
