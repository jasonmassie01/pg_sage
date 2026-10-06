package specialist

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// End to end on PostgreSQL (contract revision 1.1.0): a plan-regression
// investigation opened with a query_id is about that statement, its
// plan_regressions probe runs scoped to it, the result echoes the scope
// and names the evidence; the transcript of an investigation the model
// never ran is not_found.

type planArgsRunner struct {
	mu   sync.Mutex
	args []probes.Args
}

func (r *planArgsRunner) Run(_ context.Context, id probes.ID, a probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	if id != probes.PlanRegressions {
		return res
	}
	r.mu.Lock()
	r.args = append(r.args, a)
	r.mu.Unlock()
	res.Status = probes.StatusOK
	res.Rows = []probes.Row{{"queryid": a.QueryID, "previous_plan_hash": "v1:a",
		"current_plan_hash": "v1:b", "plan_flipped": true,
		"flipped_at":   time.Now().Add(-10 * time.Minute).UTC(),
		"before_calls": int64(20), "before_mean_ms": 1.0,
		"after_calls": int64(20), "after_mean_ms": 40.0}}
	return res
}

func TestLive_QueryScopedPlanInvestigation(t *testing.T) {
	runner := &planArgsRunner{}
	f := newLiveFixtureWith(t, runner)
	ctx := context.Background()
	w := call(t, f.handler, "POST", base+"/databases/orders/investigations", f.tokens["read"],
		`{"symptom":{"summary":"checkout slow"},"family":"plan_regression",
		"query_id":-9007199254740993}`)
	var open OpenResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &open) != nil {
		t.Fatalf("open %d %s", w.Code, w.Body.String())
	}
	if open.QueryScope == nil || open.QueryScope.QueryID != "-9007199254740993" ||
		open.QueryScope.Applied != QueryAppliedProbes ||
		open.Investigation.Subject != "queryid -9007199254740993" {
		t.Fatalf("open %s", w.Body.String())
	}
	if err := f.coord.Investigate(ctx, sre.UUID(open.Investigation.ID)); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	args := append([]probes.Args(nil), runner.args...)
	runner.mu.Unlock()
	if len(args) != 1 || args[0].QueryID != -9007199254740993 {
		t.Fatalf("plan_regressions ran with %+v", args)
	}
	w = call(t, f.handler, "GET", open.Links.Result, f.tokens["read"], "")
	var r Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("result %d %s", w.Code, w.Body.String())
	}
	q := r.QueryScope
	if r.Outcome != "concluded" || r.RootCause == nil || q == nil || !q.RootMatches ||
		len(q.EvidenceIDs) != 1 || q.QueryID != "-9007199254740993" {
		t.Fatalf("result %s", w.Body.String())
	}
	if r.Investigator != nil {
		t.Fatalf("no model ran, no investigator section: %+v", r.Investigator)
	}
	w = call(t, f.handler, "GET", base+"/databases/orders/investigations/"+
		open.Investigation.ID+"/transcript", f.tokens["read"], "")
	if w.Code != 404 || errorBody(t, w).Code != "not_found" {
		t.Fatalf("transcript of a run without the investigator: %d %s", w.Code,
			w.Body.String())
	}
}
