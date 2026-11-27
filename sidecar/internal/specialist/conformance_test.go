package specialist_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// Conformance (mcp-02): a scripted generalist SRE agent (the flow an AWS
// DevOps Agent, Azure SRE Agent, Datadog Bits or incident.io agent runs)
// drives pg_sage over MCP: open, poll, result, request the remediation,
// wait for the decision. The harness asserts the order of calls, that
// every answer's next step is the one the agent takes, and that the agent
// never gains approval: only a person's decision moves the held change.

var errNotServed = errors.New("not served by the conformance backend")

// agentBackend serves the specialist tools from the real contract service
// (through the production MCP adapter) and the request status of the
// change a remediation request queued.
type agentBackend struct {
	*mcp.SpecialistService
	mu       sync.Mutex
	requests map[string]mcp.RequestRecord
}

func (b *agentBackend) GetPolicy(context.Context, mcp.PolicyRequest) (mcp.PolicyResult, error) {
	return mcp.PolicyResult{}, errNotServed
}

func (b *agentBackend) ProposePolicyChange(context.Context,
	mcp.PolicyProposalRequest) (mcp.PolicyProposalResult, error) {
	return mcp.PolicyProposalResult{}, errNotServed
}

func (b *agentBackend) RequestChange(context.Context, mcp.ChangeRequest) (mcp.ChangeResult,
	error) {
	return mcp.ChangeResult{}, errNotServed
}

func (b *agentBackend) GetLedger(context.Context, mcp.LedgerRequest) (mcp.LedgerResult,
	error) {
	return mcp.LedgerResult{}, errNotServed
}

// SpecialistCall queues the approval row a requested remediation becomes,
// owned by the requesting agent's principal, as the executor's queue does.
func (b *agentBackend) SpecialistCall(ctx context.Context, tool string,
	caller mcp.SpecialistCaller, database string, args json.RawMessage) (any, error) {
	out, err := b.SpecialistService.SpecialistCall(ctx, tool, caller, database, args)
	if resp, ok := out.(specialist.RemediationResponse); ok && err == nil &&
		resp.Verdict == "queued_for_approval" {
		p, _ := mcp.PrincipalFromContext(ctx)
		b.set(fmt.Sprintf("change_approval:%d", resp.ApprovalQueueID), mcp.RequestRecord{
			State: "pending", Outcome: "waiting for a person in Actions",
			OwnerPrincipalID: p.PrincipalID})
	}
	return out, err
}

func (b *agentBackend) RequestStatus(_ context.Context, q mcp.RequestQuery) (
	mcp.RequestRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.requests[q.Kind+":"+q.ID]
	if !ok {
		return mcp.RequestRecord{}, mcp.ErrRequestNotFound
	}
	return rec, nil
}

func (b *agentBackend) set(handle string, rec mcp.RequestRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests[handle] = rec
}

// decide is a person's decision on the held change.
func (b *agentBackend) decide(handle, state, outcome string, final bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.requests[handle]
	rec.State, rec.DecidedByKind, rec.Outcome, rec.Final = state, "person", outcome, final
	b.requests[handle] = rec
}

// hub stands for the live-event stream.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]bool
}

func (h *hub) Subscribe() (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{}, 1)
	h.subs[ch] = true
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *hub) publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type ordersOnly struct{}

func (ordersOnly) Databases() []mcp.DatabaseRef {
	return []mcp.DatabaseRef{{Name: "orders", ID: 1}}
}

// agentPrincipal is the generalist agent's MCP token: read and propose,
// never approve, bound to an agent principal.
var agentPrincipal = mcp.Principal{Actor: "token:tok-azure", Kind: mcp.KindAgent,
	Scopes: []mcp.Scope{mcp.ScopeRead, mcp.ScopePropose}, Databases: []string{"orders"},
	TokenID: "tok-azure", Name: "Azure SRE Agent", PrincipalID: "agp_azuresreagent00000"}

// scriptedAgent records every call it makes, in order.
type scriptedAgent struct {
	t      *testing.T
	server *mcp.Server
	ctx    context.Context
	calls  []string
}

type toolAnswer struct {
	content map[string]any
	isError bool
}

func (a *scriptedAgent) call(tool string, args map[string]any) toolAnswer {
	a.t.Helper()
	a.calls = append(a.calls, tool)
	rawArgs, err := json.Marshal(args)
	if err != nil {
		a.t.Fatal(err)
	}
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool +
		`","arguments":` + string(rawArgs) + `}}`
	var resp struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(a.server.Handle(a.ctx, json.RawMessage(req))))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil || resp.Error != nil {
		a.t.Fatalf("%s: protocol error %+v %v", tool, resp.Error, err)
	}
	return toolAnswer{content: resp.Result.StructuredContent, isError: resp.Result.IsError}
}

func (a *scriptedAgent) mustSucceed(tool string, args map[string]any) map[string]any {
	a.t.Helper()
	got := a.call(tool, args)
	if got.isError {
		a.t.Fatalf("%s failed: %v", tool, got.content)
	}
	return got.content
}

func newConformance(t *testing.T) (*scriptedAgent, *specialist.ConformanceFixture,
	*agentBackend, *hub) {
	t.Helper()
	fx := specialist.NewConformanceFixture(t)
	backend := &agentBackend{SpecialistService: &mcp.SpecialistService{Service: fx.Service()},
		requests: map[string]mcp.RequestRecord{}}
	events := &hub{subs: map[chan struct{}]bool{}}
	server := mcp.NewServer(backend).WithDirectory(ordersOnly{}).WithDecisionEvents(events)
	agent := &scriptedAgent{t: t, server: server,
		ctx: mcp.WithPrincipal(context.Background(), agentPrincipal)}
	return agent, fx, backend, events
}

func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

// nextTool is the tool and arguments an answer points the agent at.
func nextTool(t *testing.T, v any) (string, map[string]any) {
	t.Helper()
	m := obj(t, v)
	return m["tool"].(string), obj(t, m["arguments"])
}

func TestConformance_GeneralistAgentSessionOverMCP(t *testing.T) {
	agent, fx, backend, events := newConformance(t)
	invID := openAndConclude(t, agent, fx)
	remID := readCitedResult(t, agent, invID)
	refuseApproval(t, agent, invID, remID)
	handle, status, wait := requestRemediation(t, agent, invID, remID)
	waitForPerson(t, agent, backend, events, handle, status, wait)
	want := []string{"specialist_open_investigation", "specialist_investigation_status",
		"specialist_investigation_status", "specialist_investigation_status",
		"specialist_investigation_result", "sre_review_investigation",
		"specialist_request_remediation", "specialist_request_remediation",
		"get_request_status", "wait_for_decision", "get_request_status"}
	if strings.Join(agent.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", agent.calls, want)
	}
	if got := fx.Requested(); len(got) != 1 {
		t.Fatalf("exactly one remediation request reached the gate: %v", got)
	}
}

// openAndConclude: open, then poll status until the investigation ends.
func openAndConclude(t *testing.T, agent *scriptedAgent,
	fx *specialist.ConformanceFixture) string {
	t.Helper()
	open := agent.mustSucceed("specialist_open_investigation", map[string]any{
		"database": "orders", "family": "lock_blocking",
		"symptom":      map[string]any{"summary": "checkout p99 at 4 s"},
		"external_ref": map[string]any{"system": "azure_sre_agent", "id": "inc-1"}})
	inv := obj(t, open["investigation"])
	invID := inv["id"].(string)
	if open["created"] != true || inv["state"] != "queued" || inv["terminal"] != false {
		t.Fatalf("open %v", open)
	}
	poll := map[string]any{"database": "orders", "investigation_id": invID}
	phases := []string{}
	for _, step := range []func(){func() {}, func() { fx.Run(invID) },
		func() { fx.Conclude(invID) }} {
		step()
		st := agent.mustSucceed("specialist_investigation_status", poll)
		phases = append(phases, st["phase"].(string))
		if n, _ := st["poll_after_seconds"].(json.Number).Int64(); !obj(t,
			st["investigation"])["terminal"].(bool) && n < 1 {
			t.Fatalf("a running investigation says when to poll again: %v", st)
		}
	}
	if strings.Join(phases, ",") != "queued,collecting_evidence,complete" {
		t.Fatalf("phases %v", phases)
	}
	return invID
}

// readCitedResult: the result is cited and names one requestable action.
func readCitedResult(t *testing.T, agent *scriptedAgent, invID string) string {
	t.Helper()
	r := agent.mustSucceed("specialist_investigation_result",
		map[string]any{"database": "orders", "investigation_id": invID})
	s := obj(t, r["summary_for_agent"])
	if r["outcome"] != "concluded" || s["status"] != "concluded" ||
		!strings.Contains(s["root_cause"].(string), "pid 4242") ||
		s["why_no_action"] != nil {
		t.Fatalf("result summary %v", s)
	}
	evidence := s["evidence"].([]any)
	if len(evidence) == 0 || len(obj(t, obj(t, evidence[0])["numbers"])) == 0 {
		t.Fatalf("the root cause cites numeric evidence: %v", evidence)
	}
	actions := s["actions"].([]any)
	if len(actions) != 1 || obj(t, actions[0])["requestable"] != true {
		t.Fatalf("actions %v", actions)
	}
	return obj(t, actions[0])["remediation_id"].(string)
}

// refuseApproval: the agent cannot approve, review or force anything.
func refuseApproval(t *testing.T, agent *scriptedAgent, invID, remID string) {
	t.Helper()
	got := agent.call("sre_review_investigation", map[string]any{"database": "orders",
		"investigation_id": invID, "verdict": "confirmed"})
	errObj := obj(t, got.content["error"])
	if !got.isError || errObj["reason"] != "approval_reserved_for_humans" {
		t.Fatalf("an approve-scope tool must be refused: %v", got.content)
	}
	got = agent.call("specialist_request_remediation", map[string]any{"database": "orders",
		"investigation_id": invID, "remediation_id": remID, "approve": true})
	if !got.isError || obj(t, got.content["error"])["reason"] != "invalid_arguments" {
		t.Fatalf("a request carrying approve must be refused: %v", got.content)
	}
	for _, tool := range agent.server.Tools() {
		if scope, _ := mcp.RequiredScope(tool.Name, json.RawMessage(`{}`)); strings.Contains(
			tool.Name, "approve") && scope != mcp.ScopeApprove {
			t.Errorf("%s approves at %s scope", tool.Name, scope)
		}
	}
}

// requestRemediation: the request is held for a person, with a handle,
// a status call and a wait call.
func requestRemediation(t *testing.T, agent *scriptedAgent, invID, remID string) (string,
	any, any) {
	t.Helper()
	r := agent.mustSucceed("specialist_request_remediation", map[string]any{
		"database": "orders", "investigation_id": invID, "remediation_id": remID,
		"reason": "incident inc-1"})
	handle := fmt.Sprintf("change_approval:%d", specialist.ConformanceQueueID)
	if r["verdict"] != "queued_for_approval" || r["handle"] != handle ||
		!strings.Contains(fmt.Sprint(r["approval_url"]), "queue=91") {
		t.Fatalf("held remediation %v", r)
	}
	if tool, args := nextTool(t, r["status"]); tool != "get_request_status" ||
		args["handle"] != handle {
		t.Fatalf("status %v", r["status"])
	}
	if tool, _ := nextTool(t, r["wait"]); tool != "wait_for_decision" {
		t.Fatalf("wait %v", r["wait"])
	}
	return handle, r["status"], r["wait"]
}

// waitForPerson: pending, then a person approves while the agent waits,
// then the change is verified; nothing is decided by the agent.
func waitForPerson(t *testing.T, agent *scriptedAgent, backend *agentBackend, events *hub,
	handle string, status, wait any) {
	t.Helper()
	tool, args := nextTool(t, status)
	st := agent.mustSucceed(tool, args)
	if st["state"] != "pending" || st["decided_by_kind"] != "" {
		t.Fatalf("pending %v", st)
	}
	if next, _ := nextTool(t, st["next"]); next != "wait_for_decision" {
		t.Fatalf("pending next %v", st["next"])
	}
	tool, args = nextTool(t, wait)
	args["timeout_seconds"] = 10
	go func() {
		for events.count() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		backend.decide(handle, "approved", "approved by user 3", false)
		events.publish()
	}()
	got := agent.mustSucceed(tool, args)
	if got["state"] != "approved" || got["decided_by_kind"] != "person" {
		t.Fatalf("decision %v", got)
	}
	backend.decide(handle, "verified", "ran and verified", true)
	tool, args = nextTool(t, status)
	final := agent.mustSucceed(tool, args)
	if final["state"] != "verified" || final["decided_by_kind"] != "person" ||
		final["next"] != nil {
		t.Fatalf("verified %v", final)
	}
}

func TestConformance_ReadOnlyAgentCannotRequestARemediation(t *testing.T) {
	agent, fx, _, _ := newConformance(t)
	agent.ctx = mcp.WithPrincipal(context.Background(), mcp.Principal{
		Actor: "token:tok-dd", Kind: mcp.KindAgent, Scopes: []mcp.Scope{mcp.ScopeRead},
		Databases: []string{"orders"}, TokenID: "tok-dd", Name: "Datadog Bits",
		PrincipalID: "agp_datadogbits0000000"})
	invID := openAndConclude(t, agent, fx)
	remID := readCitedResult(t, agent, invID)
	got := agent.call("specialist_request_remediation", map[string]any{
		"database": "orders", "investigation_id": invID, "remediation_id": remID})
	if !got.isError || obj(t, got.content["error"])["reason"] != "scope_required" ||
		len(fx.Requested()) != 0 {
		t.Fatalf("a read token requested a remediation: %v %v", got.content, fx.Requested())
	}
}

func TestConformance_AnotherAgentCannotSeeTheHeldChange(t *testing.T) {
	agent, fx, _, _ := newConformance(t)
	invID := openAndConclude(t, agent, fx)
	remID := readCitedResult(t, agent, invID)
	_, status, _ := requestRemediation(t, agent, invID, remID)
	other := *agent
	other.ctx = mcp.WithPrincipal(context.Background(), mcp.Principal{
		Actor: "token:tok-other", Kind: mcp.KindAgent,
		Scopes:    []mcp.Scope{mcp.ScopeRead, mcp.ScopePropose},
		Databases: []string{"orders"}, TokenID: "tok-other", Name: "Other agent",
		PrincipalID: "agp_otheragent00000000"})
	tool, args := nextTool(t, status)
	got := other.call(tool, args)
	if !got.isError || obj(t, got.content["error"])["reason"] != "not_found" {
		t.Fatalf("another agent read the held change: %v", got.content)
	}
}
