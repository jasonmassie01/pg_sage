package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Sage SRE statistics maintenance tools (playbooks-06). An operator who
// reviews a finished investigation later refreshes its table evidence,
// proposes extended statistics as an explicit hypothesis, and queues the
// proposal for a person's approval. The server resolves the cited
// evidence itself, so no call carries an evidence id or hash. Rollback
// drops only the object pg_sage created and needs the approve scope.

var sreMaintenanceToolNames = map[string]bool{"sre_refresh_table_evidence": true,
	"sre_propose_statistics": true, "sre_request_statistics": true,
	"sre_rollback_statistics": true}

// ErrAdminRequired refuses a statistics rollback by anyone but an admin
// person (the REST rollback is admin-only too).
var ErrAdminRequired = errors.New("statistics rollback needs an admin person with a user id")

// SREMaintenanceRequest is one statistics maintenance call.
type SREMaintenanceRequest struct {
	Database        string   `json:"database,omitempty"`
	InvestigationID string   `json:"investigation_id,omitempty"`
	ProposalID      string   `json:"proposal_id,omitempty"`
	Schema          string   `json:"schema,omitempty"`
	Table           string   `json:"table,omitempty"`
	Columns         []string `json:"columns,omitempty"`
	Kinds           []string `json:"kinds,omitempty"`
	Rationale       string   `json:"rationale,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

// SREMaintenanceBackend serves the statistics maintenance tools.
type SREMaintenanceBackend interface {
	RefreshTableEvidence(context.Context, SREMaintenanceRequest) (any, error)
	ProposeStatistics(context.Context, SREMaintenanceRequest) (any, error)
	RequestStatistics(context.Context, SREMaintenanceRequest) (any, error)
	RollbackStatistics(context.Context, SREMaintenanceRequest) (any, error)
}

func sreMaintenanceTools() []Tool {
	schema := func(properties string, required ...string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` + properties +
			`},"required":["` + strings.Join(required, `","`) +
			`"],"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"},`
	inv := `"investigation_id":{"type":"string","format":"uuid"},`
	table := `"schema":{"type":"string"},"table":{"type":"string"}`
	proposal := `"proposal_id":{"type":"string","format":"uuid"}`
	statistics := table + `,"columns":{"type":"array","items":{"type":"string"},` +
		`"minItems":2,"maxItems":8},"kinds":{"type":"array","items":{"type":"string",` +
		`"enum":["dependencies","ndistinct","mcv"]}},"rationale":{"type":"string",` +
		`"description":"the operator's correlation hypothesis, 10 to 2048 characters"}`
	return []Tool{
		{Name: "sre_refresh_table_evidence",
			InputSchema: schema(db+inv+table, "investigation_id", "schema", "table")},
		{Name: "sre_propose_statistics",
			InputSchema: schema(db+inv+statistics, "investigation_id", "schema", "table",
				"columns", "rationale")},
		{Name: "sre_request_statistics", InputSchema: schema(db+proposal, "proposal_id")},
		{Name: "sre_rollback_statistics",
			InputSchema: schema(db+proposal+`,"reason":{"type":"string"}`, "proposal_id",
				"reason")},
	}
}

// maintenanceArgsValid checks the fields each tool takes; decodeStrict
// has already refused fields no tool takes.
func maintenanceArgsValid(name string, r SREMaintenanceRequest) bool {
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	table := !blank(r.InvestigationID) && !blank(r.Schema) && !blank(r.Table) &&
		r.ProposalID == "" && r.Reason == ""
	switch name {
	case "sre_refresh_table_evidence":
		return table && len(r.Columns) == 0 && len(r.Kinds) == 0 && r.Rationale == ""
	case "sre_propose_statistics":
		return table && len(r.Columns) > 0 && !blank(r.Rationale)
	}
	byProposal := !blank(r.ProposalID) && r.InvestigationID == "" && r.Schema == "" &&
		r.Table == "" && len(r.Columns) == 0 && len(r.Kinds) == 0 && r.Rationale == ""
	if name == "sre_rollback_statistics" {
		return byProposal && !blank(r.Reason)
	}
	return name == "sre_request_statistics" && byProposal && r.Reason == ""
}

func (s *Server) callSREMaintenanceTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(SREMaintenanceBackend)
	if !ok {
		return nil, failure(-32603, "statistics maintenance unavailable")
	}
	var req SREMaintenanceRequest
	if !decodeStrict(raw, &req) || !maintenanceArgsValid(name, req) {
		return nil, failure(-32602, "invalid arguments")
	}
	call := map[string]func(context.Context, SREMaintenanceRequest) (any, error){
		"sre_refresh_table_evidence": backend.RefreshTableEvidence,
		"sre_propose_statistics":     backend.ProposeStatistics,
		"sre_request_statistics":     backend.RequestStatistics,
		"sre_rollback_statistics":    backend.RollbackStatistics,
	}[name]
	result, err := call(ctx, req)
	if err != nil {
		return nil, sreMaintenanceFailure(err)
	}
	return toolSuccess(result), nil
}

// sreMaintenanceFailure tells the operator why a refusal happened: the
// refusal texts of the maintenance service name the failed check.
func sreMaintenanceFailure(err error) *rpcError {
	switch {
	case errors.Is(err, ErrAdminRequired):
		return failure(codeScopeRequired, err.Error())
	case errors.Is(err, sreaction.ErrProposalState):
		return failure(-32009, err.Error())
	case errors.Is(err, sre.ErrRefreshTooSoon):
		return failure(-32029, "evidence was refreshed less than a minute ago; retry "+
			"after a minute")
	case errors.Is(err, sre.ErrMetadataUnavailable):
		return sreActionFailure(err)
	case errors.Is(err, sre.ErrInvalidRequest):
		return failure(-32602, "invalid request: "+err.Error())
	}
	return sreActionFailure(err)
}

// RollbackApprover is the user id a statistics rollback is recorded
// under: the bound principal must be an admin person, and its user id
// comes from its token's owner or its "user:<id>" actor.
func RollbackApprover(ctx context.Context) (int, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.Kind == KindAgent || p.Role != "admin" || !p.Has(ScopeApprove) {
		return 0, ErrAdminRequired
	}
	if p.OwnerUserID > 0 {
		return p.OwnerUserID, nil
	}
	id, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(p.Actor), "user:"))
	if err != nil || id <= 0 || !strings.HasPrefix(strings.TrimSpace(p.Actor), "user:") {
		return 0, ErrAdminRequired
	}
	return id, nil
}

// RefreshTableEvidence serves sre_refresh_table_evidence.
func (backend *ProductionBackend) RefreshTableEvidence(ctx context.Context,
	request SREMaintenanceRequest) (any, error) {
	if backend.dependencies.Maintenance == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Maintenance.RefreshTableEvidence(ctx, request)
}

// ProposeStatistics serves sre_propose_statistics.
func (backend *ProductionBackend) ProposeStatistics(ctx context.Context,
	request SREMaintenanceRequest) (any, error) {
	if backend.dependencies.Maintenance == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Maintenance.ProposeStatistics(ctx, request)
}

// RequestStatistics serves sre_request_statistics.
func (backend *ProductionBackend) RequestStatistics(ctx context.Context,
	request SREMaintenanceRequest) (any, error) {
	if backend.dependencies.Maintenance == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Maintenance.RequestStatistics(ctx, request)
}

// RollbackStatistics serves sre_rollback_statistics.
func (backend *ProductionBackend) RollbackStatistics(ctx context.Context,
	request SREMaintenanceRequest) (any, error) {
	if backend.dependencies.Maintenance == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Maintenance.RollbackStatistics(ctx, request)
}
