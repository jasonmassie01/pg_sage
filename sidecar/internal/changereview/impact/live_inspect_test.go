//go:build cgo

package impact

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

func (e *env) inspect(sql, origin string) changereview.Inspection {
	e.t.Helper()
	ins, err := e.eng.InspectStatement(e.ctx, sql, origin)
	if err != nil {
		e.t.Fatalf("InspectStatement(%q): %v", sql, err)
	}
	held := map[string]bool{}
	for _, ev := range ins.Evidence {
		held[ev.ID] = true
		if !ev.Verify() {
			e.t.Errorf("evidence %s does not verify", ev.ID)
		}
	}
	if ins.Statement.EvidenceID == "" || !held[ins.Statement.EvidenceID] {
		e.t.Fatalf("statement evidence %q is not among the inspection's evidence",
			ins.Statement.EvidenceID)
	}
	return ins
}

// InspectStatement: stages 1, 3 and 5 for one statement (the reasoner's
// inspect_statement tool).
func TestInspectReadMatchesTheWorkloadStatement(t *testing.T) {
	e := newEnv(t)
	ins := e.inspect("SELECT email FROM rv.rv_orders WHERE id = 42 /* app='x' */",
		changereview.OriginModelExtracted)
	s := ins.Statement
	if !ins.Parsed || !ins.Planned || ins.ParseError != "" || ins.PlanError != "" {
		t.Fatalf("inspection %+v", ins)
	}
	if s.MatchedQueryID == nil || *s.MatchedQueryID != e.f.Q1.ID || s.Fingerprint == "" ||
		s.Origin != changereview.OriginModelExtracted || s.Kind != "select" ||
		s.PlanSummary == "" || len(s.Relations) != 1 || s.Relations[0] != "rv.rv_orders" {
		t.Fatalf("statement %+v, want Q1 %d matched and planned", s, e.f.Q1.ID)
	}
	other := e.inspect("SELECT status FROM rv.rv_orders WHERE customer_id = $1",
		changereview.OriginModelExtracted)
	if other.Statement.MatchedQueryID != nil || !other.Planned {
		t.Fatalf("a new statement shape matched queryid %v", other.Statement.MatchedQueryID)
	}
	if other.Statement.EvidenceID == s.EvidenceID {
		t.Fatal("two statements share one evidence id")
	}
}

func TestInspectWritePlansItsReadShape(t *testing.T) {
	e := newEnv(t)
	ins := e.inspect("UPDATE rv.rv_orders SET status = $1 WHERE id = $2",
		changereview.OriginModelExtracted)
	if !ins.Parsed || !ins.Planned || ins.Statement.MatchedQueryID == nil ||
		*ins.Statement.MatchedQueryID != e.f.Q2.ID {
		t.Fatalf("UPDATE inspection %+v, want planned and matched to Q2", ins)
	}
	ins = e.inspect("UPDATE rv.rv_orders o SET status = 'x' FROM rv.rv_items i "+
		"WHERE i.order_id = o.id", changereview.OriginModelExtracted)
	if !ins.Parsed || ins.Planned || !strings.Contains(ins.PlanError, "read shape") {
		t.Fatalf("UPDATE ... FROM has no read shape: %+v", ins)
	}
}

func TestInspectDDLRunsTheRules(t *testing.T) {
	e := newEnv(t)
	ins := e.inspect("ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address",
		changereview.OriginModelTranslated)
	var cr03 bool
	for _, f := range ins.Findings {
		cr03 = cr03 || (f.RuleID == RuleColumnReferenced &&
			f.Severity == changereview.SeverityCritical)
	}
	if !ins.Parsed || !ins.Planned || !cr03 || ins.Statement.Kind != "rename_column" {
		t.Fatalf("translated rename: %+v", ins)
	}
}

func TestInspectRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	ins := e.inspect("SELEC email FRM rv.rv_orders", changereview.OriginModelExtracted)
	if ins.Parsed || ins.Planned || ins.ParseError == "" {
		t.Fatalf("garbage parsed: %+v", ins)
	}
	for _, c := range []struct{ sql, origin string }{
		{"", changereview.OriginModelExtracted},
		{"   ", changereview.OriginModelExtracted},
		{"SELECT 1", "made_up"},
		{"SELECT 1; SELECT 2", changereview.OriginModelExtracted},
		{strings.Repeat("x", 100_001), changereview.OriginModelExtracted},
	} {
		_, err := e.eng.InspectStatement(e.ctx, c.sql, c.origin)
		if !errors.Is(err, changereview.ErrInvalid) {
			t.Errorf("InspectStatement(%.20q, %q) = %v, want ErrInvalid", c.sql, c.origin, err)
		}
	}
}

func TestConcurrentAnalysesAgree(t *testing.T) {
	e := newEnv(t)
	const sql = "ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address;\n" +
		"CREATE INDEX CONCURRENTLY rv_orders_cust_idx ON rv.rv_orders (customer_id)"
	want := e.analyze(sql)
	var wg sync.WaitGroup
	results := make([]changereview.Analysis, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.eng.Analyze(e.ctx, input(sql))
		}()
	}
	wg.Wait()
	for i, a := range results {
		if errs[i] != nil {
			t.Fatalf("analysis %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(rulesOf(a), rulesOf(want)) ||
			!reflect.DeepEqual(a.Expectations.PostConditions, want.Expectations.PostConditions) {
			t.Fatalf("analysis %d differs:%s\nwant:%s", i, describe(a), describe(want))
		}
	}
}

func rulesOf(a changereview.Analysis) []string {
	var out []string
	for _, f := range a.Findings {
		out = append(out, f.RuleID+"/"+f.Severity+"/"+strings.Join(f.EvidenceIDs, ","))
	}
	return out
}

// Integration: the engine as the service's analyzer, end to end on real PG.
func TestServiceStoresTheEngineReview(t *testing.T) {
	e := newEnv(t)
	svc := e.service(nil)
	r, created, err := svc.Submit(e.ctx, changereview.Caller{Actor: "test", Source: "cli"},
		changereview.Request{SQL: "ALTER TABLE rv.rv_orders RENAME COLUMN email TO " +
			"email_address", WaitSeconds: 30})
	if err != nil || !created || r.State != changereview.StateComplete {
		t.Fatalf("Submit: %v created=%v state=%s (%s)", err, created, r.State, r.FailureReason)
	}
	if r.Verdict != changereview.VerdictRisky || r.ParseMode != changereview.ParseAST {
		t.Fatalf("stored verdict %s (%s), want risky", r.Verdict, r.ParseMode)
	}
	var cr03 bool
	for _, f := range r.Findings {
		cr03 = cr03 || f.RuleID == RuleColumnReferenced
	}
	for _, ev := range r.Evidence {
		if !ev.Verify() {
			t.Errorf("stored evidence %s does not verify", ev.ID)
		}
	}
	if !cr03 || len(r.Expectations.PostConditions) != 1 ||
		len(r.Expectations.WatchQueryIDs) == 0 {
		t.Fatalf("stored review: CR-03 %v, expectations %+v", cr03, r.Expectations)
	}
	if !strings.Contains(r.Markdown, "CR-03") {
		t.Fatalf("the rendered comment does not show CR-03")
	}
}
