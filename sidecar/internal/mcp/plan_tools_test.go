package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// plan-03b: agents propose plans over MCP and read them; nobody approves a
// plan over MCP. propose_plan stores a proposed plan with origin agent and
// the principal id after the plan-01a validation, within a per-principal
// open-plan limit (default 5).

// planBackend is a fake plan store: it validates proposals with the real
// validator over citations that resolve for finding 11 only, and keeps
// what it stored.
type planBackend struct {
	recordingBackend
	mu       sync.Mutex
	stored   []changeplan.Plan
	limits   []int
	database string
	err      error
}

type planCitations struct{}

func (planCitations) FindingExists(_ context.Context, id int64) (bool, error) {
	return id == 11, nil
}

func (planCitations) EvidenceExists(context.Context, string, string) (bool, error) {
	return false, nil
}

func (planCitations) TargetCalls(context.Context, []int64, time.Duration) (int64, error) {
	return 5000, nil
}

// The merge with plan-02d widened changeplan.Resolver; these plans cite
// no settings and no managed changes.
func (planCitations) SettingExists(context.Context, string) (bool, error) {
	return false, nil
}

func (planCitations) ManagedChangeExists(context.Context, int64) (bool, error) {
	return false, nil
}

func (b *planBackend) ListPlans(_ context.Context, database string,
	f changeplan.ListFilter) ([]changeplan.Plan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.database = database
	if b.err != nil {
		return nil, b.err
	}
	var out []changeplan.Plan // nil when none, as the store answers
	for _, p := range b.stored {
		if f.Status == "" || p.Status == f.Status {
			out = append(out, p)
		}
	}
	return out, nil
}

func (b *planBackend) GetPlan(_ context.Context, database string,
	id int64) (changeplan.Detail, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.database = database
	for _, p := range b.stored {
		if p.ID == id {
			return changeplan.Detail{Plan: p}, nil
		}
	}
	return changeplan.Detail{}, changeplan.ErrNotFound
}

func (b *planBackend) ProposePlan(ctx context.Context, database string, p changeplan.Plan,
	limit int) (changeplan.Plan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.database, b.limits = database, append(b.limits, limit)
	if b.err != nil {
		return changeplan.Plan{}, b.err
	}
	problems, err := changeplan.Validate(ctx, p, planCitations{})
	if err != nil {
		return changeplan.Plan{}, err
	}
	if len(problems) > 0 {
		return changeplan.Plan{}, &changeplan.InvalidPlanError{Problems: problems}
	}
	open := 0
	for _, s := range b.stored {
		if s.OriginRef == p.OriginRef {
			open++
		}
	}
	if open >= limit {
		return changeplan.Plan{}, &changeplan.OpenPlanLimitError{Open: open, Limit: limit}
	}
	p.ID, p.PlanHash = int64(len(b.stored)+1), changeplan.PlanHash(p)
	b.stored = append(b.stored, p)
	return p, nil
}

func (b *planBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.stored)
}

const planStep = `{"ordinal":1,"kind":"action","action_type":"create_index_concurrently",` +
	`"proposed_sql":"CREATE INDEX CONCURRENTLY idx_a ON public.orders (customer_id)",` +
	`"rollback_sql":"DROP INDEX CONCURRENTLY public.idx_a","finding_id":11,` +
	`"target_queryids":[101],"metric":"blocks_per_call","expected_direction":"down",` +
	`"expected_source":"hypopg","deadline_minutes":60}`

func planJSON(steps ...string) string {
	return `{"goal":"cut blocks per call on the order lookups","scope":{"objects":` +
		`["public.orders"]},"steps":[` + strings.Join(steps, ",") + `]}`
}

func proposeArgs(plan string) string {
	return `{"database":"orders","plan":` + plan + `}`
}

func planAgentCtx() context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:t1", Kind: KindAgent,
		Scopes: []Scope{ScopeRead, ScopePropose}, TokenID: "t1", PrincipalID: "agp_alpha"})
}

func TestPlanToolsAreListedAndNoneApproves(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range NewServer(&planBackend{}).Tools() {
		names[tool.Name] = true
		if strings.Contains(tool.Name, "plan") && (strings.Contains(tool.Name, "approve") ||
			strings.Contains(tool.Name, "decide")) {
			t.Fatalf("an MCP tool decides plans: %s", tool.Name)
		}
	}
	for _, want := range []string{"propose_plan", "list_plans", "get_plan"} {
		require.True(t, names[want], want)
	}
	scope, _ := RequiredScope("propose_plan", json.RawMessage(`{}`))
	require.Equal(t, ScopePropose, scope)
	for _, read := range []string{"list_plans", "get_plan"} {
		scope, _ = RequiredScope(read, json.RawMessage(`{}`))
		require.Equal(t, ScopeRead, scope, read)
	}
}

func TestProposePlanFromAnAgentTokenStoresAProposedAgentPlan(t *testing.T) {
	backend := &planBackend{}
	response := invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.count())
	p := backend.stored[0]
	require.Equal(t, changeplan.OriginAgent, p.Origin)
	require.Equal(t, "agp_alpha", p.OriginRef)
	require.Equal(t, changeplan.StatusProposed, p.Status)
	require.Equal(t, "mcp:token:t1", p.CreatedBy)
	require.Equal(t, "orders", backend.database)
	require.Equal(t, changeplan.DefaultOpenPlansPerPrincipal, backend.limits[0])
	out := structuredContent(t, response)
	require.Equal(t, "proposed", out["status"])
	require.Equal(t, "agent", out["origin"])
	require.True(t, strings.Contains(out["note"].(string), "never approve"))
}

func TestProposePlanAsAStringOfJSONIsAccepted(t *testing.T) {
	backend := &planBackend{}
	quoted, _ := json.Marshal(planJSON(planStep))
	response := invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("propose_plan", proposeArgs(string(quoted))))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.count())
}

func planCode(t *testing.T, response protocolResponse) string {
	t.Helper()
	result, _ := response.Result.(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	failure, _ := structured["error"].(map[string]any)
	code, _ := failure["plan_code"].(string)
	return code
}

func TestProposePlanRefusesBadInputAndCreatesNothing(t *testing.T) {
	many := make([]string, 0, changeplan.MaxSteps+1)
	for i := 1; i <= changeplan.MaxSteps+1; i++ {
		many = append(many, strings.Replace(planStep, `"ordinal":1`,
			`"ordinal":`+strconv.Itoa(i), 1))
	}
	fenced, _ := json.Marshal("```json\n" + planJSON(planStep) + "\n```")
	malformed, _ := json.Marshal(`{"goal": "x", "steps": [`)
	trailing, _ := json.Marshal(planJSON(planStep) + " and more")
	unknownFinding := strings.Replace(planStep, `"finding_id":11`, `"finding_id":99`, 1)
	withField := func(field string) string {
		return `{"goal":"g",` + field + `,"steps":[` + planStep + `]}`
	}
	cases := map[string]struct{ args, code string }{
		"malformed JSON":      {proposeArgs(string(malformed)), "plan_malformed"},
		"text after the JSON": {proposeArgs(string(trailing)), "plan_malformed"},
		"markdown fences":     {proposeArgs(string(fenced)), "plan_fenced"},
		"empty body":          {`{"database":"orders"}`, "plan_empty"},
		"empty object":        {proposeArgs(`{}`), "plan_empty"},
		"empty string":        {proposeArgs(`""`), "plan_empty"},
		"null plan":           {proposeArgs(`null`), "plan_empty"},
		"an array":            {proposeArgs(`[1]`), "plan_malformed"},
		"51 steps":            {proposeArgs(planJSON(many...)), "invalid_plan"},
		"unknown finding":     {proposeArgs(planJSON(unknownFinding)), "invalid_plan"},
		"agent sets status": {proposeArgs(withField(`"status":"approved"`)),
			"plan_malformed"},
		"agent sets origin": {proposeArgs(withField(`"origin":"person"`)),
			"plan_malformed"},
		"agent sets plan hash": {proposeArgs(withField(`"plan_hash":"ab"`)),
			"plan_malformed"},
	}
	for name, c := range cases {
		backend := &planBackend{}
		response := invoke(t, NewServer(backend), planAgentCtx(), toolCall("propose_plan", c.args))
		require.Equal(t, codeInvalidParams, response.Error.Code, name)
		require.Equal(t, c.code, planCode(t, response), name)
		require.Equal(t, 0, backend.count(), name)
	}
}

func TestProposePlanInvalidPlanNamesTheProblems(t *testing.T) {
	backend := &planBackend{}
	bad := strings.Replace(planStep, `"finding_id":11`, `"finding_id":99`, 1)
	response := invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("propose_plan", proposeArgs(planJSON(bad))))
	result, _ := response.Result.(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	failure, _ := structured["error"].(map[string]any)
	problems, _ := failure["problems"].([]any)
	require.True(t, len(problems) > 0, "no problems listed: %v", failure)
	first, _ := problems[0].(map[string]any)
	require.Equal(t, changeplan.CodeEvidenceUnresolved, first["code"])
}

func TestProposePlanOpenPlanLimitBoundary(t *testing.T) {
	backend := &planBackend{}
	server := NewServer(backend)
	for i := 1; i <= changeplan.DefaultOpenPlansPerPrincipal; i++ {
		response := invoke(t, server, planAgentCtx(),
			toolCall("propose_plan", proposeArgs(planJSON(planStep))))
		require.Empty(t, response.Error.Code, "plan %d: %s", i, response.Error.Message)
	}
	response := invoke(t, server, planAgentCtx(),
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	require.Equal(t, codeRateLimited, response.Error.Code)
	require.Equal(t, "open_plan_limit", planCode(t, response))
	require.Equal(t, changeplan.DefaultOpenPlansPerPrincipal, backend.count())
}

func TestPlanToolsMapStoreErrors(t *testing.T) {
	cases := map[error]int{
		changeplan.ErrNotFound:   codeNotFound,
		errors.New("db is down"): codeInternal,
		&changeplan.CapError{}:   codeConflict,
	}
	for err, code := range cases {
		backend := &planBackend{err: err}
		response := invoke(t, NewServer(backend), planAgentCtx(),
			toolCall("propose_plan", proposeArgs(planJSON(planStep))))
		require.Equal(t, code, response.Error.Code, err.Error())
		require.False(t, strings.Contains(response.Error.Message, "db is down"), "leaked")
	}
	plain := invoke(t, NewServer(&recordingBackend{}), planAgentCtx(),
		toolCall("list_plans", `{"database":"orders"}`))
	require.Equal(t, codeInternal, plain.Error.Code)
}

func TestListPlansEmptyIsAnEmptyListAndGetPlanReads(t *testing.T) {
	backend := &planBackend{}
	response := invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("list_plans", `{"database":"orders"}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	out := structuredContent(t, response)
	plans, ok := out["plans"].([]any)
	require.True(t, ok && len(plans) == 0, "plans = %#v, want []", out["plans"])
	invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	response = invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("get_plan", `{"database":"orders","plan_id":1}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	plan := objectMap(t, structuredContent(t, response)["plan"])
	require.Equal(t, "cut blocks per call on the order lookups", plan["goal"])
	response = invoke(t, NewServer(backend), planAgentCtx(),
		toolCall("get_plan", `{"database":"orders","plan_id":2}`))
	require.Equal(t, codeNotFound, response.Error.Code)
	for _, args := range []string{`{"plan_id":0}`, `{"plan_id":-1}`, `{}`,
		`{"plan_id":1,"plan":{}}`} {
		response = invoke(t, NewServer(backend), planAgentCtx(), toolCall("get_plan", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	for _, args := range []string{`{"status":"dancing"}`, `{"limit":0}`, `{"limit":501}`,
		`{"plan_id":3}`} {
		response = invoke(t, NewServer(backend), planAgentCtx(), toolCall("list_plans", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
}

func TestPlanToolsAuthorization(t *testing.T) {
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	backend := &planBackend{}
	response := invoke(t, NewServer(backend), viewer,
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	require.Equal(t, codeScopeRequired, response.Error.Code)
	readOnlyAgent := WithPrincipal(context.Background(), Principal{Actor: "token:t2",
		Kind: KindAgent, Scopes: []Scope{ScopeRead}, PrincipalID: "agp_ro"})
	response = invoke(t, NewServer(backend), readOnlyAgent,
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	require.Equal(t, codeScopeRequired, response.Error.Code)
	response = invoke(t, NewServer(backend), context.Background(),
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	require.Equal(t, codeScopeRequired, response.Error.Code)
	require.Equal(t, 0, backend.count())
	// The stdio client is an agent: it holds no approve scope, and no plan
	// tool takes one.
	stdio := WithPrincipal(context.Background(), Principal{Actor: "stdio", Kind: KindAgent,
		Scopes: []Scope{ScopeRead, ScopePropose, ScopeApprove}})
	require.False(t, mustPrincipal(stdio).Has(ScopeApprove))
	for _, tool := range []string{"approve_plan", "plan_approve", "decide_plan"} {
		response = invoke(t, NewServer(backend), stdio, toolCall(tool, `{"plan_id":1}`))
		require.Equal(t, codeInvalidParams, response.Error.Code, tool)
		require.True(t, strings.Contains(response.Error.Message, "Unknown tool"), tool)
	}
	// A person proposing over MCP is recorded as a person, under the same limit.
	person := WithPrincipal(context.Background(), Principal{Actor: "user:9", Role: "operator"})
	response = invoke(t, NewServer(backend), person,
		toolCall("propose_plan", proposeArgs(planJSON(planStep))))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, changeplan.OriginPerson, backend.stored[0].Origin)
	require.Equal(t, "user:9", backend.stored[0].OriginRef)
}

func mustPrincipal(ctx context.Context) Principal {
	p, _ := PrincipalFromContext(ctx)
	return p
}
