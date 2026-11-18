package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/specialist"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// A held result tells the agent where a person decides (approval_url),
// what to poll (status) and the item's handle (mcp-01a).

// No concurrent access tests beyond the inbox fake's lock: shaping a
// result is a pure function of the call and the backend's answer.

func heldContent(t *testing.T, server *Server, ctx context.Context, tool,
	args string) map[string]any {
	t.Helper()
	response := invoke(t, server, ctx, toolCall(tool, args))
	require.Zero(t, response.Error.Code, response.Error.Message)
	content := structuredContent(t, response)
	text := objectMap(t, response.Result)["content"].([]any)[0].(map[string]any)["text"]
	var fromText map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.(string)), &fromText))
	require.Equal(t, content["handle"], fromText["handle"], "the text block says the same")
	return content
}

// intentRecorder answers the named intent tools with the change result.
type intentRecorder struct{ *recordingBackend }

func (b *intentRecorder) RequestIntent(_ context.Context, _ string,
	_ json.RawMessage) (any, error) {
	return b.changeResult, nil
}

// A queued change creates no action-queue item on the intent path; its
// handle names the ledger decision, and status (mcp-01b) reads it with
// get_request_status, never the change approvals.
func TestHeldChangeCarriesApprovalURLStatusAndHandle(t *testing.T) {
	for _, tool := range []string{"request_change", "optimize_query"} {
		backend := &recordingBackend{changeResult: ChangeResult{Decision: "queued",
			EvidenceID: "ev-9", Reason: "approval_required"}}
		backendWithIntents := &intentRecorder{recordingBackend: backend}
		args := map[string]string{
			"request_change": `{"intent":{"kind":"optimize_query","query_id":7}}`,
			"optimize_query": `{"goal":"latency","query_id":7}`}[tool]
		server := NewServer(backendWithIntents).WithDirectory(fleetOf("orders"))
		content := heldContent(t, server, operatorContext(context.Background()), tool, args)
		require.Equal(t, "queued", content["decision"], tool)
		require.Equal(t, "#/actions?tab=decisions&database=orders", content["approval_url"],
			tool)
		require.Equal(t, "change:ev-9", content["handle"], tool)
		status := objectMap(t, content["status"])
		require.Equal(t, toolGetRequestStatus, status["tool"], tool)
		require.Equal(t, map[string]any{"database": "orders", "handle": "change:ev-9"},
			status["arguments"], tool)
		require.Equal(t, toolWaitForDecision, objectMap(t, content["wait"])["tool"], tool)
	}
}

func TestParkedChangePointsAtHeldBack(t *testing.T) {
	backend := &recordingBackend{changeResult: ChangeResult{Decision: "parked",
		EvidenceID: "ev-3", Reason: "outside_policy"}}
	content := heldContent(t, changeServer(backend), operatorContext(context.Background()),
		"request_change", `{"intent":{"kind":"optimize_query","query_id":7}}`)
	require.Equal(t, "#/actions?tab=decisions&database=orders", content["approval_url"])
	require.Equal(t, "change:ev-3", content["handle"])
	status := objectMap(t, content["status"])
	require.Equal(t, toolGetRequestStatus, status["tool"])
	require.Equal(t, map[string]any{"database": "orders", "handle": "change:ev-3"},
		status["arguments"])
}

func TestChangeThatIsNotHeldCarriesNoHeldFields(t *testing.T) {
	for _, decision := range []string{"granted", "blocked", "recommend_only", ""} {
		backend := &recordingBackend{changeResult: ChangeResult{Decision: decision,
			EvidenceID: "ev-1"}}
		response := invoke(t, changeServer(backend), operatorContext(context.Background()),
			toolCall("request_change", `{"intent":{"kind":"optimize_query","query_id":7}}`))
		content := structuredContent(t, response)
		for _, key := range []string{"approval_url", "status", "handle"} {
			require.NotContains(t, content, key, "%q decision", decision)
		}
	}
	held := &recordingBackend{changeResult: ChangeResult{Decision: "queued"}}
	content := structuredContent(t, invoke(t, changeServer(held),
		operatorContext(context.Background()), toolCall("request_change",
			`{"intent":{"kind":"optimize_query","query_id":7}}`)))
	require.NotContains(t, content, "handle", "no evidence id, no handle")
	require.Contains(t, content, "approval_url", "a person still decides")
}

func TestPolicyProposalIsHeldForAPerson(t *testing.T) {
	backend := &recordingBackend{proposalResult: PolicyProposalResult{ProposalID: 42}}
	content := heldContent(t, changeServer(backend), operatorContext(context.Background()),
		"propose_policy_change", `{"delta":{"budgets":{"storage_bytes":0}}}`)
	require.Equal(t, json.Number("42"), content["proposal_id"])
	require.Equal(t, "#/agents?tab=policy&proposal=42&database=orders", content["approval_url"])
	require.Equal(t, "policy_exception:42", content["handle"])
	status := objectMap(t, content["status"])
	require.Equal(t, map[string]any{"database": "orders",
		"handle": "policy_exception:42"}, status["arguments"])
	none := &recordingBackend{}
	empty := structuredContent(t, invoke(t, changeServer(none),
		operatorContext(context.Background()), toolCall("propose_policy_change",
			`{"delta":{"x":1}}`)))
	require.NotContains(t, empty, "approval_url", "proposal_id 0 is no proposal")
}

// remediationBackend answers specialist_request_remediation with the
// contract's own response type, as production does.
type remediationBackend struct {
	recordingBackend
	response specialist.RemediationResponse
}

func (b *remediationBackend) SpecialistCall(_ context.Context, _ string, _ SpecialistCaller,
	_ string, _ json.RawMessage) (any, error) {
	return b.response, nil
}

const remediationArgs = `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11",` +
	`"remediation_id":"cancel.0123abcd"}`

func TestRemediationQueuedCarriesTheApprovalQueueHandle(t *testing.T) {
	for _, verdict := range []string{"queued_for_approval", "already_requested"} {
		backend := &remediationBackend{response: specialist.RemediationResponse{
			Verdict: verdict, ApprovalQueueID: 7, Reason: "approval_required"}}
		content := heldContent(t, NewServer(backend).WithDirectory(fleetOf("orders")),
			operatorContext(context.Background()), "specialist_request_remediation",
			remediationArgs)
		require.Equal(t, "#/actions?tab=pending&queue=7&database=orders",
			content["approval_url"], verdict)
		require.Equal(t, "change_approval:7", content["handle"], verdict)
		status := objectMap(t, content["status"])
		require.Equal(t, toolGetRequestStatus, status["tool"])
	}
	noQueue := &remediationBackend{response: specialist.RemediationResponse{
		Verdict: "queued_for_approval"}}
	content := heldContent(t, NewServer(noQueue).WithDirectory(fleetOf("orders")),
		operatorContext(context.Background()), "specialist_request_remediation",
		remediationArgs)
	require.NotContains(t, content, "wait", "no queue id, no handle to wait on")
	require.Equal(t, toolGetInbox, objectMap(t, content["status"])["tool"],
		"without a handle, status stays the inbox")
	blocked := &remediationBackend{response: specialist.RemediationResponse{Verdict: "blocked"}}
	content = structuredContent(t, invoke(t, NewServer(blocked).WithDirectory(
		fleetOf("orders")), operatorContext(context.Background()),
		toolCall("specialist_request_remediation", remediationArgs)))
	require.NotContains(t, content, "status", "a blocked request waits on nobody")
}

func TestQueuedCapabilityKeepsItsApprovalURLAndAddsStatus(t *testing.T) {
	b := &grantToolBackend{result: CapabilityResult{Verdict: VerdictQueueApproval,
		RequestID: 4, ApprovalURL: "/agents/x"}}
	content := heldContent(t, NewServer(b), grantAgentCtx(), toolRequestCapability,
		capabilityArgs)
	require.Equal(t, "/agents/x", content["approval_url"], "the backend's URL is kept")
	require.Equal(t, "agent_request:4", content["handle"])
	status := objectMap(t, content["status"])
	require.Equal(t, map[string]any{"database": "orders",
		"handle": "agent_request:4"}, status["arguments"])
}

// decidingInbox is an inbox a person empties by deciding its items.
type decidingInbox struct {
	recordingBackend
	mu    sync.Mutex
	items []map[string]any
	asked []InboxRequest
}

func (b *decidingInbox) Inbox(_ context.Context, req InboxRequest) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, req)
	return map[string]any{"items": append([]map[string]any{}, b.items...)}, nil
}

// RequestStatus (mcp-01b) is pending while an item waits, then approved;
// the caller made every request.
func (b *decidingInbox) RequestStatus(ctx context.Context, _ RequestQuery) (RequestRecord,
	error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) > 0 {
		return RequestRecord{State: "pending", OwnerActor: ActorFromContext(ctx)}, nil
	}
	return RequestRecord{State: "approved", DecidedByKind: byPerson, Final: true,
		OwnerActor: ActorFromContext(ctx)}, nil
}

func (b *decidingInbox) decide() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = nil
}

// pollStatus calls the tool and arguments a held result's status names
// and returns the state it reports.
func pollStatus(t *testing.T, server *Server, status map[string]any) any {
	t.Helper()
	args := mustJSON(t, status["arguments"])
	content := structuredContent(t, invoke(t, server, operatorContext(context.Background()),
		toolCall(status["tool"].(string), args)))
	return content["state"]
}

func TestHeldProposalStatusShowsItWaitingThenDecided(t *testing.T) {
	backend := &decidingInbox{items: []map[string]any{{"kind": "policy_exception",
		"id": "42", "database": "orders"}}}
	backend.proposalResult = PolicyProposalResult{ProposalID: 42}
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	content := heldContent(t, server, operatorContext(context.Background()),
		"propose_policy_change", `{"delta":{"x":1}}`)
	status := objectMap(t, content["status"])
	require.Equal(t, "pending", pollStatus(t, server, status),
		"held: the item waits on a person")
	backend.decide()
	require.Equal(t, "approved", pollStatus(t, server, status), "decided by a person")
}
