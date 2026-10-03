package executor

import (
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Found while fixing the stale approval: an optimizer finding's identity is
// "schema.table|<index definition>" (C05). Its change lease used that
// identity as a catalog name, the lease target did not resolve ("invalid
// identifier") and every autonomous optimizer CREATE INDEX was recorded as
// a failed action instead of running. The lease is on the table.
func TestLeaseSpecForOptimizerFindingLeasesTheTable(t *testing.T) {
	f := optimizerFinding("verified")
	request := findingRequest(f, false)
	spec, ok := leaseSpecFor(ActionIntent{Request: request, Lease: &f})
	if !ok || !reflect.DeepEqual(spec.Targets, []string{"public.orders"}) {
		t.Fatalf("lease spec = %+v (%v), want the table public.orders", spec, ok)
	}
	f.Detail["table"] = "sales.orders"
	spec, _ = leaseSpecFor(ActionIntent{Request: findingRequest(f, false), Lease: &f})
	if !reflect.DeepEqual(spec.Targets, []string{"sales.orders"}) {
		t.Fatalf("lease targets = %v, want the detail's table", spec.Targets)
	}
	// The gate request keeps the full identity (ledger and limits).
	if !reflect.DeepEqual(request.TargetObjs, []string{f.ObjectIdentifier}) {
		t.Fatalf("request targets = %v, want the finding identity", request.TargetObjs)
	}
}

// Other findings keep their identifier as the lease target.
func TestLeaseSpecForOtherFindingKeepsItsTarget(t *testing.T) {
	f := analyzer.Finding{Category: "missing_fk_index", ObjectIdentifier: "public.items",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY items_fk ON public.items (order_id)"}
	spec, ok := leaseSpecFor(ActionIntent{Request: findingRequest(f, false), Lease: &f})
	if !ok || !reflect.DeepEqual(spec.Targets, []string{"public.items"}) {
		t.Fatalf("lease spec = %+v (%v), want public.items", spec, ok)
	}
	empty := analyzer.Finding{Category: "missing_index",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY i ON public.t (c)"}
	spec, ok = leaseSpecFor(ActionIntent{Lease: &empty})
	if !ok || len(spec.Targets) != 0 {
		t.Fatalf("lease spec = %+v (%v), want no targets for an empty identity", spec, ok)
	}
}
