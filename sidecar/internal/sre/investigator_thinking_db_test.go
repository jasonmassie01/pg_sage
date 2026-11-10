package sre

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Thinking models at the investigation's token cap (sre-04): every call
// reserves maxInvestigatorReasoning on top of the loop's estimate, the
// loop counts the same allowance, and a call the investigation's own cap
// refuses (a resumed run's earlier reservations) is asked once more as
// final-only under what remains. Daily-allocation refusals stay stops.
// No concurrent-access case: one investigation's calls are sequential
// under its lease (investigator_concurrency_db_test.go covers leases).

const thinkingModelName = "gemini-3-pro-preview"

// wireRequest is the part of a chat completions request the tests read.
type wireRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice string `json:"tool_choice"`
	MaxTokens  int    `json:"max_tokens"`
}

func requestOf(t *testing.T, body string) wireRequest {
	t.Helper()
	var r wireRequest
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Errorf("request body: %v", err)
	}
	return r
}

func (r wireRequest) names() string {
	out := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		out = append(out, t.Function.Name)
	}
	return strings.Join(out, ",")
}

// estimate is the loop's estimate of the request as sent.
func (r wireRequest) estimate(stepTokens int) int {
	msgs := make([]llm.Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	specs := make([]llm.ToolSpec, 0, len(r.Tools))
	for _, t := range r.Tools {
		specs = append(specs, llm.ToolSpec{Name: t.Function.Name,
			Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	return agentloop.EstimateTokens(msgs, specs, stepTokens)
}

func idleSubmit(t *testing.T) fakeReply {
	return submit(func(body string) invFinal {
		return invFinal{Outcome: "agree", Claims: []invClaim{idleClaim(aliasOf(t, body,
			probes.LockGraph, "ok"))}}
	})
}

// firstEstimate is the investigator's first request estimate with every
// tool offered, measured on a run of its own.
func firstEstimate(t *testing.T, plans map[string]InvestigatorPlan) int {
	t.Helper()
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, submitFixed(invFinal{Outcome: "agree"}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{config: InvestigatorConfig{Plans: plans}})
	startAndRun(t, ctx, c, lockTrigger("inv-think-cal"))
	return requestOf(t, m.body(t, 0)).estimate(plans[PlanNarrow].StepTokens)
}

func TestInvestigator_ThinkingModelConcludesAtItsTokenCap(t *testing.T) {
	plans := DefaultInvestigatorPlans()
	p := plans[PlanNarrow]
	full := firstEstimate(t, plans)
	// Two calls fit without the reasoning allowance but not with it; after
	// a first read only 1500 tokens would remain, short of any reservation.
	p.MaxTokens = int64(2*full + 2048)
	plans[PlanNarrow] = p
	st, _, ctx := liveStore(t, budgetLimits())
	read := call(ToolGraphState, `{}`)
	final := idleSubmit(t)
	m := newFakeModel(t, func(w http.ResponseWriter, body string) {
		if requestOf(t, body).names() == ToolSubmit {
			final(w, body)
			return
		}
		read(&usageWriter{ResponseWriter: w, usage: int(p.MaxTokens) - 1500}, body)
	})
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(),
		namedModel(m.srv.URL, thinkingModelName, 1_000_000),
		invOptions{config: InvestigatorConfig{Plans: plans}})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-think-cap"))
	assertIdleRoot(t, st, inv)
	run := transcriptOf(t, inv)
	if run.Stop != "final" || inv.Summary.ModelConclusion == nil {
		t.Fatalf("stop %s (%s), conclusion %v; want a stored conclusion at the cap %d",
			run.Stop, run.StopDetail, inv.Summary.ModelConclusion, p.MaxTokens)
	}
	if got := requestOf(t, m.body(t, 0)).names(); got != ToolSubmit || m.calls() != 1 {
		t.Fatalf("first request offered %q in %d calls, want only %s", got, m.calls(),
			ToolSubmit)
	}
	if run.Tokens > int(p.MaxTokens) {
		t.Fatalf("tokens %d over the cap %d", run.Tokens, p.MaxTokens)
	}
}

func TestInvestigator_DailyAllocationRefusalStopsWithoutARetry(t *testing.T) {
	l := budgetLimits()
	l.DatabaseDailyTokens = 1000
	st, _, ctx := liveStore(t, l)
	m := newFakeModel(t, submitFixed(invFinal{Outcome: "agree"}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(),
		namedModel(m.srv.URL, thinkingModelName, 1_000_000), invOptions{})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-think-daily"))
	assertIdleRoot(t, st, inv)
	run := transcriptOf(t, inv)
	if run.Stop != "budget_exhausted" || run.ModelCalls != 1 || m.calls() != 0 {
		t.Fatalf("stop %s after %d loop calls and %d provider calls; want "+
			"budget_exhausted after one refused call", run.Stop, run.ModelCalls, m.calls())
	}
	if !strings.Contains(run.StopDetail, "daily allocation") {
		t.Fatalf("stop detail %q does not name the daily allocation", run.StopDetail)
	}
	if got := reservationsOf(t, st, inv); got[InvestigatorCallerKind] != 0 {
		t.Fatalf("reservations = %v, want none", got)
	}
}

var capMsgs = []llm.Message{{Role: "system", Content: "You investigate."},
	{Role: "user", Content: "Find the cause."}}

func finalSpecs() []llm.ToolSpec {
	f := investigatorFinal()
	return []llm.ToolSpec{{Name: f.Name, Description: f.Description,
		Parameters: f.Parameters}}
}

func fullSpecs() []llm.ToolSpec {
	return append([]llm.ToolSpec{{Name: ToolGraphState, Description: "Read the graph.",
		Parameters: json.RawMessage(`{"type":"object"}`)}}, finalSpecs()...)
}

// cappedSession is an investigator session on a thinking model whose
// investigation already holds all of its cap but headroom tokens above a
// final-only call's input and reasoning, as a resumed run finds it.
func cappedSession(t *testing.T, subject string, headroom int64,
	script ...fakeReply) (*investigatorSession, *fakeModel, Investigation) {
	t.Helper()
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, script...)
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(),
		namedModel(m.srv.URL, thinkingModelName, 1_000_000), invOptions{})
	lease := claimed(t, st, subject)
	inv, err := st.Get(ctx, lease.Scope, lease.InvestigationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	plan := DefaultInvestigatorPlans()[PlanNarrow]
	input := int64(agentloop.EstimateTokens(capMsgs, finalSpecs(), 0))
	held := plan.MaxTokens - input - maxInvestigatorReasoning - headroom
	if _, err := st.ReserveInvestigator(ctx, lease, TokenRequest{Input: held - 1,
		Output: 1, RequestKey: "earlier-run"}, plan.MaxTokens); err != nil {
		t.Fatalf("earlier run's reservation: %v", err)
	}
	return &investigatorSession{c: c, plan: plan, lease: lease, inv: inv}, m, inv
}

func stepOpts(choice llm.ToolChoice) llm.ToolOptions {
	return llm.ToolOptions{MaxTokens: 1024, Timeout: 5 * time.Second, ToolChoice: choice}
}

func TestInvestigatorModel_CapRefusalRetriesOnceAsFinalOnly(t *testing.T) {
	for _, headroom := range []int64{600, 256} {
		s, m, inv := cappedSession(t, "pid 9101-"+itoa(headroom), headroom,
			submitFixed(invFinal{Outcome: "agree"}))
		res, err := investigatorModel{s: s}.ChatWithTools(t.Context(), capMsgs,
			fullSpecs(), stepOpts(""))
		if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != ToolSubmit {
			t.Fatalf("headroom %d: retry = %+v (%v), want the conclusion", headroom, res, err)
		}
		req := requestOf(t, m.body(t, 0))
		if m.calls() != 1 || req.names() != ToolSubmit || req.ToolChoice != "required" ||
			req.MaxTokens != int(headroom)+maxInvestigatorReasoning {
			t.Fatalf("headroom %d: %d calls offering %q (%s) with max_tokens %d", headroom,
				m.calls(), req.names(), req.ToolChoice, req.MaxTokens)
		}
		rows := reservations(t, s.c.store.(*PostgresStore).pool, inv)
		if len(rows) != 2 || rows[1].output != headroom ||
			rows[1].reasoning != maxInvestigatorReasoning {
			t.Fatalf("headroom %d: reservations %+v, want the earlier run's and one "+
				"final-only call of %d answer tokens", headroom, rows, headroom)
		}
	}
}

func TestInvestigatorModel_CapRefusalWithoutRoomStopsOnTheBudget(t *testing.T) {
	cases := []struct {
		name     string
		headroom int64
		tools    []llm.ToolSpec
		choice   llm.ToolChoice
	}{
		{"below the 256 floor", 255, fullSpecs(), ""},
		{"already final-only", 600, finalSpecs(), llm.ToolChoiceRequired},
		{"JSON actions", 600, nil, ""},
	}
	for i, tc := range cases {
		s, m, inv := cappedSession(t, "pid 9102-"+itoa(int64(i)), tc.headroom,
			submitFixed(invFinal{Outcome: "agree"}))
		_, err := investigatorModel{s: s}.ChatWithTools(t.Context(), capMsgs, tc.tools,
			stepOpts(tc.choice))
		if !errors.Is(err, llm.ErrBudgetExhausted) ||
			!strings.Contains(err.Error(), "investigator tokens") {
			t.Fatalf("%s: %v, want the investigation cap's budget stop", tc.name, err)
		}
		if m.calls() != 0 || len(reservations(t, s.c.store.(*PostgresStore).pool,
			inv)) != 1 {
			t.Fatalf("%s: %d provider calls; a refused call must reach no provider",
				tc.name, m.calls())
		}
	}
}

func TestInvestigatorModel_RetryOffersOnlyTheFinal(t *testing.T) {
	s, m, _ := cappedSession(t, "pid 9103", 600, call(ToolGraphState, `{}`))
	_, err := investigatorModel{s: s}.ChatWithTools(t.Context(), capMsgs, fullSpecs(),
		stepOpts(""))
	if !errors.Is(err, llm.ErrMalformedToolCall) || m.calls() != 1 {
		t.Fatalf("a read in the final-only retry = %v after %d calls, want it refused",
			err, m.calls())
	}
}

// A daily allocation that refuses the full call but would admit the
// final-only retry: only the investigation's own cap may be retried, so
// no provider call is made and the daily allocation is named.
func TestInvestigatorModel_DailyRefusalIsNotRetriedWhenTheRetryWouldFit(t *testing.T) {
	full := int64(agentloop.EstimateTokens(capMsgs, fullSpecs(), 0)) + 1024 +
		maxInvestigatorReasoning
	l := budgetLimits()
	l.DatabaseDailyTokens = full - 1
	st, _, ctx := liveStore(t, l)
	m := newFakeModel(t, submitFixed(invFinal{Outcome: "agree"}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(),
		namedModel(m.srv.URL, thinkingModelName, 1_000_000), invOptions{})
	lease := claimed(t, st, "pid 9104")
	inv, err := st.Get(ctx, lease.Scope, lease.InvestigationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	s := &investigatorSession{c: c, plan: DefaultInvestigatorPlans()[PlanNarrow],
		lease: lease, inv: inv}
	_, err = investigatorModel{s: s}.ChatWithTools(t.Context(), capMsgs, fullSpecs(),
		stepOpts(""))
	if !errors.Is(err, llm.ErrBudgetExhausted) ||
		!strings.Contains(err.Error(), "database daily allocation") {
		t.Fatalf("daily refusal = %v, want the database daily allocation's budget stop",
			err)
	}
	if m.calls() != 0 || len(reservations(t, st.pool, inv)) != 0 {
		t.Fatalf("%d provider calls after a daily refusal; it must not be retried",
			m.calls())
	}
}
