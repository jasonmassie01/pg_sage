package whynot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// approvalInputs is a fresh database with one unused-index finding whose
// candidate the gate asks an operator to approve, an open window, room in
// the budget, every grant and no stop.
func approvalInputs() Inputs {
	return Inputs{Now: testNow, Database: "orders", Provider: "postgres",
		Observed: Observed{LastSnapshot: testNow.Add(-time.Minute), Freshness: "fresh",
			Interval: time.Minute},
		Findings:   Findings{Open: 1, Category: "unused_index", FindingID: 42},
		LLMEnabled: true, Candidates: 1,
		Verdicts: []Verdict{{ActionType: "drop_index", Decision: "queue_for_approval",
			Reason: "approval_required"}},
		Window:     &Window{Open: true},
		Budget:     &Budget{Used: 1, Limit: 5},
		Privileges: &Privileges{Role: "sage", StatStatements: true, QueryTextVisible: true},
		TrustLevel: "advisory",
	}
}

func linkOf(t *testing.T, res Result, name LinkName) Link {
	t.Helper()
	for _, link := range res.Links {
		if link.Name == name {
			return link
		}
	}
	t.Fatalf("no %s link in %+v", name, res.Links)
	return Link{}
}

func statuses(res Result) []Status {
	out := make([]Status, len(res.Links))
	for i, link := range res.Links {
		out[i] = link.Status
	}
	return out
}

// Happy path: observed, finding and candidate pass, the policy link fails
// with "asks first: approval required" and links the finding's card.
func TestChainApprovalRequiredLinksTheCard(t *testing.T) {
	res := Chain(context.Background(), approvalInputs())
	if len(res.Links) != 10 {
		t.Fatalf("links = %d, want 10", len(res.Links))
	}
	for i, name := range Links() {
		if res.Links[i].Name != name {
			t.Fatalf("link %d = %s, want %s", i, res.Links[i].Name, name)
		}
	}
	want := []Status{Pass, Pass, Pass, Fail, Pass, Pass, Pass, Pass, NotApplicable, Pass}
	got := statuses(res)
	for i := range want {
		if i != 8 && got[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
	}
	policy := linkOf(t, res, LinkPolicy)
	if res.FirstFail != LinkPolicy || !policy.FirstFail ||
		!strings.Contains(strings.ToLower(policy.Sentence), "asks first: approval required") {
		t.Fatalf("policy link = %+v, first fail %s", policy, res.FirstFail)
	}
	if policy.Fix == nil || policy.Fix.Kind != FixUILink ||
		policy.Fix.Value != "#/cases?finding=42&database=orders" {
		t.Fatalf("policy fix = %+v, want the card link", policy.Fix)
	}
	if len(res.Failing) != 1 || res.Failing[0] != LinkPolicy {
		t.Fatalf("failing = %v", res.Failing)
	}
}

// The verification wait is not reached by an approval verdict: unknown,
// never a pass.
func TestChainUnreachedGateLinkIsUnknownNotPass(t *testing.T) {
	res := Chain(context.Background(), approvalInputs())
	if got := linkOf(t, res, LinkVerification); got.Status != Unknown {
		t.Fatalf("verification = %+v, want unknown", got)
	}
}

// Nil/empty: no snapshot yet fails the first link with the due time.
func TestChainNotObservedYet(t *testing.T) {
	in := approvalInputs()
	in.Observed = Observed{Interval: 5 * time.Minute, Freshness: "unknown"}
	res := Chain(context.Background(), in)
	link := linkOf(t, res, LinkObserved)
	due := "2026-11-17T12:05:00Z"
	if link.Status != Fail || !strings.Contains(link.Sentence,
		"Not observed yet; first snapshot due at "+due) || link.Fix == nil ||
		link.Fix.Kind != FixWaitUntil || link.Fix.Value != due {
		t.Fatalf("observed = %+v", link)
	}
	if res.FirstFail != LinkObserved {
		t.Fatalf("first fail = %s, want observed", res.FirstFail)
	}
}

func TestChainNotObservedWithoutInterval(t *testing.T) {
	in := approvalInputs()
	in.Observed = Observed{}
	link := linkOf(t, Chain(context.Background(), in), LinkObserved)
	if link.Status != Fail || link.Fix == nil || link.Fix.Kind != FixSetting {
		t.Fatalf("observed without interval = %+v", link)
	}
}

func TestChainObservedStaleAndUnknown(t *testing.T) {
	in := approvalInputs()
	in.Observed.Freshness = "stale"
	in.Observed.LastSnapshot = testNow.Add(-time.Hour)
	link := linkOf(t, Chain(context.Background(), in), LinkObserved)
	if link.Status != Fail || !strings.Contains(link.Sentence, "1h0m0s ago") ||
		link.Fix == nil || link.Fix.Value != "#/manage-databases?database=orders" {
		t.Fatalf("stale = %+v", link)
	}
	in.Observed.Freshness = ""
	if link := linkOf(t, Chain(context.Background(), in), LinkObserved); link.Status != Unknown {
		t.Fatalf("no freshness = %+v, want unknown", link)
	}
}

// Nil/empty: no findings of an LLM-dependent category with LLM off says so
// and points at the setting; the later links still run.
func TestChainLLMOff(t *testing.T) {
	in := approvalInputs()
	in.Findings = Findings{Category: "missing_index"}
	in.LLMEnabled, in.Candidates, in.Verdicts = false, 0, nil
	res := Chain(context.Background(), in)
	link := linkOf(t, res, LinkFinding)
	if link.Status != Fail || !strings.Contains(link.Sentence, "LLM is off") ||
		link.Fix == nil || link.Fix.Kind != FixSetting || link.Fix.Value != "llm.enabled=true" {
		t.Fatalf("finding = %+v", link)
	}
	if got := linkOf(t, res, LinkCandidate); got.Status != NotApplicable {
		t.Fatalf("candidate = %+v, want not_applicable", got)
	}
	if got := linkOf(t, res, LinkPolicy); got.Status != NotApplicable {
		t.Fatalf("policy = %+v, want not_applicable", got)
	}
	if got := linkOf(t, res, LinkPrivilege); got.Status != Pass {
		t.Fatalf("privilege = %+v, want pass from the probe", got)
	}
}

// A category no LLM produces, with LLM off, is plain "nothing to change".
func TestChainNoFindingsDeterministicCategory(t *testing.T) {
	in := approvalInputs()
	in.Findings = Findings{Category: "unused_index"}
	in.LLMEnabled, in.Candidates, in.Verdicts = false, 0, nil
	link := linkOf(t, Chain(context.Background(), in), LinkFinding)
	if link.Status != Fail || strings.Contains(link.Sentence, "LLM") || link.Fix != nil ||
		link.NoFix == "" {
		t.Fatalf("finding = %+v", link)
	}
}

func TestChainFindingNoLongerOpenAndAdvisory(t *testing.T) {
	in := approvalInputs()
	in.Findings = Findings{FindingID: 7}
	link := linkOf(t, Chain(context.Background(), in), LinkFinding)
	if link.Status != Fail || !strings.Contains(link.Sentence, "#7") {
		t.Fatalf("closed finding = %+v", link)
	}
	in = approvalInputs()
	in.Candidates, in.Verdicts = 0, nil
	link = linkOf(t, Chain(context.Background(), in), LinkCandidate)
	if link.Status != Fail || !strings.Contains(link.Sentence, "advisory") {
		t.Fatalf("advisory finding = %+v", link)
	}
}

// Error propagation: an explain error leaves the policy link unknown with
// the error class only, and the later links still run on their facts.
func TestChainExplainErrorIsUnknownAndLaterLinksRun(t *testing.T) {
	in := approvalInputs()
	in.Verdicts = nil
	in.PolicyErr = context.DeadlineExceeded
	in.Stop = Stop{Stopped: true}
	res := Chain(context.Background(), in)
	policy := linkOf(t, res, LinkPolicy)
	if policy.Status != Unknown || policy.Error != "timeout" {
		t.Fatalf("policy = %+v, want unknown timeout", policy)
	}
	if got := linkOf(t, res, LinkVerification); got.Status != Unknown || got.Error != "timeout" {
		t.Fatalf("verification = %+v", got)
	}
	if got := linkOf(t, res, LinkWindow); got.Status != Pass {
		t.Fatalf("window = %+v, want pass from its fact", got)
	}
	if res.FirstFail != LinkEmergencyStop {
		t.Fatalf("first fail = %s, want the stop", res.FirstFail)
	}
}

func TestChainCancelledContextMakesThePolicyUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	link := linkOf(t, Chain(ctx, approvalInputs()), LinkPolicy)
	if link.Status != Unknown || link.Error != "cancelled" {
		t.Fatalf("policy = %+v", link)
	}
}

func TestChainReadErrorsAreUnknown(t *testing.T) {
	in := approvalInputs()
	in.Findings.Err = errors.New("dial tcp 10.0.0.1: refused")
	in.PrivilegesErr = errors.New("password for role sage")
	res := Chain(context.Background(), in)
	for _, name := range []LinkName{LinkFinding, LinkCandidate, LinkPrivilege} {
		link := linkOf(t, res, name)
		if link.Status != Unknown || link.Error != "error" ||
			strings.Contains(link.Sentence, "10.0.0.1") {
			t.Fatalf("%s = %+v", name, link)
		}
	}
}

// Boundary: the stop is checked last but marked first, with the others
// still listed after it.
func TestChainEmergencyStopOverridesEarlierFailures(t *testing.T) {
	in := approvalInputs()
	in.Observed = Observed{Interval: time.Minute}
	in.Stop = Stop{Stopped: true, By: "alice", At: testNow.Add(-time.Hour)}
	res := Chain(context.Background(), in)
	if res.FirstFail != LinkEmergencyStop {
		t.Fatalf("first fail = %s, want emergency_stop", res.FirstFail)
	}
	want := []LinkName{LinkEmergencyStop, LinkObserved, LinkPolicy}
	if len(res.Failing) != len(want) {
		t.Fatalf("failing = %v, want %v", res.Failing, want)
	}
	for i := range want {
		if res.Failing[i] != want[i] {
			t.Fatalf("failing = %v, want %v", res.Failing, want)
		}
	}
	stop := linkOf(t, res, LinkEmergencyStop)
	if !stop.FirstFail || linkOf(t, res, LinkObserved).FirstFail ||
		!strings.Contains(stop.Sentence, "alice stopped it at 2026-11-17T11:00:00Z") ||
		stop.Fix == nil || stop.Fix.Kind != FixUILink {
		t.Fatalf("stop = %+v", stop)
	}
	in.Stop = Stop{Fleet: true}
	stop = linkOf(t, Chain(context.Background(), in), LinkEmergencyStop)
	if stop.Status != Fail || !strings.Contains(stop.Sentence, "fleet-wide") {
		t.Fatalf("fleet stop = %+v", stop)
	}
}

// Boundary: a budget exactly at its cap fails; one below passes.
func TestChainBudgetAtItsCapFails(t *testing.T) {
	in := approvalInputs()
	in.Budget = &Budget{Used: 5, Limit: 5, FreesAt: testNow.Add(time.Hour)}
	link := linkOf(t, Chain(context.Background(), in), LinkBudget)
	if link.Status != Fail || !strings.Contains(link.Sentence, "5 of 5") || link.Fix == nil ||
		link.Fix.Kind != FixWaitUntil || link.Fix.Value != "2026-11-17T13:00:00Z" {
		t.Fatalf("budget at cap = %+v", link)
	}
	in.Budget = &Budget{Used: 4, Limit: 5}
	if link := linkOf(t, Chain(context.Background(), in), LinkBudget); link.Status != Pass {
		t.Fatalf("budget below cap = %+v", link)
	}
	in.Budget = &Budget{Used: 0, Limit: 0}
	link = linkOf(t, Chain(context.Background(), in), LinkBudget)
	if link.Status != Fail || link.Fix == nil || link.Fix.Kind != FixUILink {
		t.Fatalf("zero budget = %+v, want fail with the policy link", link)
	}
}

// The gate's own budget verdict fails the budget link with the time its
// detail names, even when the caller's fact shows room.
func TestChainGateBudgetVerdictWinsOverTheFact(t *testing.T) {
	in := approvalInputs()
	in.Verdicts = []Verdict{{Decision: "parked", Reason: "rate_limit_exceeded",
		Detail: "performance budget full: 5 of 5 changes in the 24h window; " +
			"next frees at 2026-11-17T18:00:00Z"}}
	res := Chain(context.Background(), in)
	link := linkOf(t, res, LinkBudget)
	if link.Status != Fail || link.Reason != "rate_limit_exceeded" || link.Fix == nil ||
		link.Fix.Kind != FixWaitUntil || link.Fix.Value != "2026-11-17T18:00:00Z" {
		t.Fatalf("budget = %+v", link)
	}
	if res.FirstFail != LinkBudget {
		t.Fatalf("first fail = %s", res.FirstFail)
	}
	if got := linkOf(t, res, LinkPolicy); got.Status != Unknown {
		t.Fatalf("policy = %+v, want unknown (held at another link)", got)
	}
}

// A wait detail whose time has passed links the Held back tab instead.
func TestChainPastWaitFallsBackToHeldBack(t *testing.T) {
	in := approvalInputs()
	in.Verdicts = []Verdict{{Decision: "parked", Reason: "awaiting_verification",
		Detail: "awaiting action 6 (until 2026-11-17T11:00:00Z) on guc:work_mem"}}
	link := linkOf(t, Chain(context.Background(), in), LinkVerification)
	if link.Status != Fail || link.Fix == nil ||
		link.Fix.Value != "#/actions?tab=decisions&database=orders" {
		t.Fatalf("verification = %+v", link)
	}
}

// An execute verdict passes every gate link without a fact.
func TestChainExecuteVerdictPassesTheGateLinks(t *testing.T) {
	in := approvalInputs()
	in.Window, in.Budget, in.Privileges, in.TrustLevel = nil, nil, nil, ""
	in.Verdicts = []Verdict{{Decision: "execute", Reason: "authorized"}}
	res := Chain(context.Background(), in)
	for _, link := range res.Links {
		if link.Status != Pass {
			t.Fatalf("%s = %+v, want pass", link.Name, link)
		}
	}
	if res.FirstFail != "" || len(res.Failing) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestChainUnmappedReasonIsAPolicyFailureAsWritten(t *testing.T) {
	in := approvalInputs()
	in.Verdicts = []Verdict{{Decision: "blocked", Reason: "unknown action type"}}
	link := linkOf(t, Chain(context.Background(), in), LinkPolicy)
	if link.Status != Fail || link.Sentence != "unknown action type" || link.NoFix == "" {
		t.Fatalf("policy = %+v", link)
	}
	in.Verdicts = []Verdict{{Decision: "blocked"}}
	link = linkOf(t, Chain(context.Background(), in), LinkPolicy)
	if link.Status != Fail || !strings.Contains(link.Sentence, "no reason") {
		t.Fatalf("policy = %+v", link)
	}
}

func TestChainWindowAndTrustFacts(t *testing.T) {
	in := approvalInputs()
	in.Window = &Window{NextOpen: testNow.Add(2 * time.Hour)}
	in.TrustLevel = "observation"
	res := Chain(context.Background(), in)
	window := linkOf(t, res, LinkWindow)
	if window.Status != Fail || window.Fix == nil || window.Fix.Value != "2026-11-17T14:00:00Z" {
		t.Fatalf("window = %+v", window)
	}
	trust := linkOf(t, res, LinkTrust)
	if trust.Status != Fail || trust.Fix == nil || trust.Fix.Value != "trust.level=advisory" {
		t.Fatalf("trust = %+v", trust)
	}
	in.Window, in.TrustLevel = &Window{}, "bogus"
	res = Chain(context.Background(), in)
	if w := linkOf(t, res, LinkWindow); w.Status != Fail || w.Fix != nil || w.NoFix == "" {
		t.Fatalf("never-open window = %+v", w)
	}
	if tr := linkOf(t, res, LinkTrust); tr.Status != Fail || tr.Reason != "unknown_trust_level" {
		t.Fatalf("unknown trust = %+v", tr)
	}
}

// State transition: the same inputs with the grant added flip the
// privilege link from fail to pass; each gap names its exact fix.
func TestChainPrivilegeGaps(t *testing.T) {
	in := approvalInputs()
	in.Provider = "rds"
	in.Privileges = &Privileges{Role: "sage", StatStatements: true}
	link := linkOf(t, Chain(context.Background(), in), LinkPrivilege)
	if link.Status != Fail || link.Fix == nil || link.Fix.Kind != FixGrantSQL ||
		link.Fix.Value != "GRANT pg_read_all_stats TO sage;" ||
		!strings.Contains(link.Fix.Note, "rds_superuser") {
		t.Fatalf("hidden query text = %+v %+v", link, link.Fix)
	}
	in.Privileges.QueryTextVisible = true
	if link := linkOf(t, Chain(context.Background(), in), LinkPrivilege); link.Status != Pass {
		t.Fatalf("after the grant = %+v", link)
	}
	in.Privileges.StatStatements = false
	link = linkOf(t, Chain(context.Background(), in), LinkPrivilege)
	if link.Status != Fail || link.Fix == nil ||
		!strings.Contains(link.Fix.Value, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements") {
		t.Fatalf("missing extension = %+v", link)
	}
	in.Privileges.Replica = true
	link = linkOf(t, Chain(context.Background(), in), LinkPrivilege)
	if link.Status != Fail || link.Fix != nil || !strings.Contains(link.Sentence, "replica") {
		t.Fatalf("replica = %+v", link)
	}
}

func TestChainZeroInputs(t *testing.T) {
	res := Chain(context.Background(), Inputs{})
	if len(res.Links) != 10 || res.FirstFail != LinkObserved || res.Failing == nil {
		t.Fatalf("zero inputs = %+v", res)
	}
	if got := linkOf(t, res, LinkEmergencyStop); got.Status != Pass {
		t.Fatalf("stop = %+v", got)
	}
	if got := linkOf(t, res, LinkTrust); got.Status != NotApplicable {
		t.Fatalf("trust without level or verdict = %+v", got)
	}
}

// Concurrent access: Chain shares no state; parallel calls agree.
func TestChainConcurrentCallsAgree(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := Chain(context.Background(), approvalInputs()); res.FirstFail != LinkPolicy {
				errs <- string(res.FirstFail)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("parallel chain first fail = %s", e)
	}
}

func TestErrorClass(t *testing.T) {
	for err, want := range map[error]string{
		nil: "", context.Canceled: "cancelled", errors.New("x"): "error",
		sqlStateErr("42501"): "permission_denied", sqlStateErr("57014"): "sqlstate_57014",
	} {
		if got := ErrorClass(err); got != want {
			t.Errorf("ErrorClass(%v) = %q, want %q", err, got, want)
		}
	}
}

type sqlStateErr string

func (e sqlStateErr) Error() string    { return "sql " + string(e) }
func (e sqlStateErr) SQLState() string { return string(e) }
