package mcp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/pg-sage/sidecar/internal/agenttools"
)

// allToolsBackend implements every backend interface the server routes
// to and counts the calls that reach it, so scope and database tests can
// prove a refused call never reaches a backend.
type allToolsBackend struct {
	mu       sync.Mutex
	n        int
	tool     string
	ctxDB    string
	facts    factCall
	agent    agentCall
	intent   json.RawMessage
	change   ChangeRequest
	agentErr error
	packet   agenttools.Packet
}

type factCall struct {
	req   FactRequest
	actor string
}

type agentCall struct {
	method string
	req    any
	actor  string
}

func newAllToolsBackend() *allToolsBackend { return &allToolsBackend{} }

func (b *allToolsBackend) server() *Server { return NewServer(b) }

func (b *allToolsBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

func (b *allToolsBackend) hit(ctx context.Context, tool string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	b.tool = tool
	b.ctxDB, _ = DatabaseFromContext(ctx)
}

var allToolsArgs = map[string]string{
	"get_policy": `{}`, "propose_policy_change": `{"delta":{"x":1}}`,
	"request_change":         `{"intent":{"kind":"optimize_query","query_id":1}}`,
	"get_ledger":             `{"filter":{}}`,
	"optimize_query":         `{"goal":"latency","query_id":1}`,
	"apply_migration":        `{"table":"public.t","sql":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
	"ensure_fk_indexes":      `{"schema":"public"}`,
	"declare_table_contract": `{"table":"public.events","append_only":true}`,
	"register_consumer":      `{"slot_name":"s1","owner":"agent"}`,
	"set_maintenance_policy": `{"scope":{},"patch":{"x":1}}`,
	"get_guarantee_status":   `{}`, "get_value": `{}`,
	"sre_list_incidents":    `{}`,
	"sre_get_investigation": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_get_evidence": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11",` +
		`"evidence_id":"6f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_propose_action":    `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_request_execution": `{"proposal_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_list_slos":         `{}`, "sre_get_slo": `{"name":"checkout"}`,
	"sre_list_changes": `{}`, "sre_list_runbooks": `{}`,
	"sre_get_runbook":       `{"runbook_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_runbook_runs":      `{"runbook_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_similar_incidents": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"sre_draft_runbook":     `{"definition":{"steps":[]}}`,
	"sre_compile_runbook":   `{"text":"check replication lag"}`,
	"sre_get_autonomy":      `{}`, "sre_evaluate_autonomy": `{}`,
	"sre_downgrade_autonomy": `{"family":"wal_retention","action_class":"wal_bound",` +
		`"level":"L1","reason":"lag"}`,
	"sre_review_investigation": `{"investigation_id":"x","verdict":"accepted"}`,
	"list_facts":               `{}`,
	"propose_fact": `{"type":"test_fixture","subject_kind":"schema","subject":"test_*",` +
		`"evidence":"CI leaks these"}`,
	"decide_fact":    `{"fact_id":12,"decision":"confirm"}`,
	"list_databases": `{}`, "top_queries": `{}`, "query_sources": `{}`,
	"explain_query":  `{"query":"SELECT 1"}`,
	"whatif_index":   `{"ddl":"CREATE INDEX ON public.t (a)"}`,
	"lint_migration": `{"sql":"ALTER TABLE public.t ADD COLUMN c int"}`,
	"mark_object": `{"subject_kind":"index","subject":"public.idx_a","mark":"owned",` +
		`"evidence":"db/migrate/001_create_idx_a.rb creates it"}`,
	"get_source_fix_packet": `{"finding_id":7}`,
	"report_source_fix": `{"finding_id":7,"stage":"pr_opened",` +
		`"pr_url":"https://github.com/acme/app/pull/12"}`,
	"specialist_open_investigation":   `{"symptom":{"summary":"slow"}}`,
	"specialist_investigation_status": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"specialist_investigation_result": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
	"specialist_request_remediation": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11",` +
		`"remediation_id":"custodian.0123456789abcdef"}`,
}

func (b *allToolsBackend) validArgs(tool string) string {
	if args, ok := allToolsArgs[tool]; ok {
		return args
	}
	return `{}`
}

// Backend and IntentBackend.

func (b *allToolsBackend) GetPolicy(ctx context.Context, _ PolicyRequest) (PolicyResult,
	error) {
	b.hit(ctx, "get_policy")
	return PolicyResult{Version: 1}, nil
}

func (b *allToolsBackend) ProposePolicyChange(ctx context.Context,
	_ PolicyProposalRequest) (PolicyProposalResult, error) {
	b.hit(ctx, "propose_policy_change")
	return PolicyProposalResult{ProposalID: 1}, nil
}

func (b *allToolsBackend) RequestChange(ctx context.Context, r ChangeRequest) (ChangeResult,
	error) {
	b.hit(ctx, "request_change")
	b.change = r
	return ChangeResult{Decision: "queued"}, nil
}

func (b *allToolsBackend) GetLedger(ctx context.Context, _ LedgerRequest) (LedgerResult,
	error) {
	b.hit(ctx, "get_ledger")
	return LedgerResult{}, nil
}

func (b *allToolsBackend) RequestIntent(ctx context.Context, tool string,
	args json.RawMessage) (any, error) {
	b.hit(ctx, tool)
	b.intent = append(json.RawMessage(nil), args...)
	return map[string]any{"tool": tool}, nil
}

// Investigations, signals, actions, runbooks, autonomy.

func (b *allToolsBackend) ListInvestigations(ctx context.Context,
	_ InvestigationRequest) (any, error) {
	b.hit(ctx, "sre_list_incidents")
	return map[string]any{}, nil
}

func (b *allToolsBackend) GetInvestigation(ctx context.Context,
	_ InvestigationRequest) (any, error) {
	b.hit(ctx, "sre_get_investigation")
	return map[string]any{}, nil
}

func (b *allToolsBackend) GetEvidence(ctx context.Context, _ InvestigationRequest) (any,
	error) {
	b.hit(ctx, "sre_get_evidence")
	return map[string]any{}, nil
}

func (b *allToolsBackend) ListSLOs(ctx context.Context, _ SignalRequest) (any, error) {
	b.hit(ctx, "sre_list_slos")
	return map[string]any{}, nil
}

func (b *allToolsBackend) GetSLO(ctx context.Context, _ SignalRequest) (any, error) {
	b.hit(ctx, "sre_get_slo")
	return map[string]any{}, nil
}

func (b *allToolsBackend) ListChanges(ctx context.Context, _ SignalRequest) (any, error) {
	b.hit(ctx, "sre_list_changes")
	return map[string]any{}, nil
}

func (b *allToolsBackend) ProposeAction(ctx context.Context, _ SREActionRequest) (any,
	error) {
	b.hit(ctx, "sre_propose_action")
	return map[string]any{}, nil
}

func (b *allToolsBackend) RequestExecution(ctx context.Context, _ SREActionRequest) (any,
	error) {
	b.hit(ctx, "sre_request_execution")
	return map[string]any{}, nil
}

func (b *allToolsBackend) runbook(ctx context.Context, tool string) (any, error) {
	b.hit(ctx, tool)
	return map[string]any{}, nil
}

func (b *allToolsBackend) ListRunbooks(ctx context.Context, _ RunbookRequest) (any, error) {
	return b.runbook(ctx, "sre_list_runbooks")
}

func (b *allToolsBackend) GetRunbook(ctx context.Context, _ RunbookRequest) (any, error) {
	return b.runbook(ctx, "sre_get_runbook")
}

func (b *allToolsBackend) RunbookRuns(ctx context.Context, _ RunbookRequest) (any, error) {
	return b.runbook(ctx, "sre_runbook_runs")
}

func (b *allToolsBackend) DraftRunbook(ctx context.Context, _ RunbookRequest) (any, error) {
	return b.runbook(ctx, "sre_draft_runbook")
}

func (b *allToolsBackend) CompileRunbook(ctx context.Context, _ RunbookRequest) (any,
	error) {
	return b.runbook(ctx, "sre_compile_runbook")
}

func (b *allToolsBackend) SimilarIncidents(ctx context.Context, _ RunbookRequest) (any,
	error) {
	return b.runbook(ctx, "sre_similar_incidents")
}

func (b *allToolsBackend) GetAutonomy(ctx context.Context, _ AutonomyRequest) (any, error) {
	return b.runbook(ctx, "sre_get_autonomy")
}

func (b *allToolsBackend) DowngradeAutonomy(ctx context.Context, _ AutonomyRequest,
	_ string) (any, error) {
	return b.runbook(ctx, "sre_downgrade_autonomy")
}

func (b *allToolsBackend) ReviewInvestigation(ctx context.Context, _ AutonomyRequest,
	_ string) (any, error) {
	return b.runbook(ctx, "sre_review_investigation")
}

func (b *allToolsBackend) EvaluateAutonomy(ctx context.Context, _ AutonomyRequest) (any,
	error) {
	return b.runbook(ctx, "sre_evaluate_autonomy")
}

func (b *allToolsBackend) SpecialistCall(ctx context.Context, tool string,
	_ SpecialistCaller, _ string, _ json.RawMessage) (any, error) {
	b.hit(ctx, tool)
	return map[string]any{}, nil
}
