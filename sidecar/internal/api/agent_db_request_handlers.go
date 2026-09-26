package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// requestCreateFromBody builds a request create. allowed_regions is server
// policy (config providers.*.allowed_regions) and is never read from the
// body, so a caller cannot choose its own allowlist (G8-B05).
func requestCreateFromBody(
	r *http.Request,
	m map[string]any,
	authority *agentDBLiveAuthority,
) agentdb.RequestCreate {
	provider := str(m, "provider")
	return agentdb.RequestCreate{
		RequestID:          str(m, "request_id"),
		TenantID:           str(m, "tenant_id"),
		AgentID:            str(m, "agent_id"),
		OwnerID:            str(m, "owner_id"),
		RunID:              str(m, "run_id"),
		Purpose:            str(m, "purpose"),
		IsolationType:      str(m, "requested_isolation_type"),
		DatabaseName:       str(m, "database_name"),
		Provider:           provider,
		IdempotencyKey:     firstString(r.Header.Get("Idempotency-Key"), str(m, "idempotency_key")),
		BudgetUSD:          float(m, "budget_usd"),
		BackupRequired:     boolValue(m, "backup_required"),
		DataClassification: str(m, "data_classification"),
		MaskingPolicyID:    str(m, "masking_policy_id"),
		Region:             str(m, "region"),
		AllowedRegions:     authority.allowedRegions(provider),
		ApprovalSLASeconds: integer(m, "approval_sla_seconds"),
		Body:               m,
	}
}

func (a *agentDBLiveAuthority) maxTTLSeconds(provider string) int {
	if a == nil {
		return 0
	}
	return a.config.Providers[agentdb.NormalizeProviderName(provider)].MaxTTLSeconds
}

func (a *agentDBLiveAuthority) allowedRegions(provider string) []string {
	if a == nil {
		return nil
	}
	p, ok := a.config.Providers[agentdb.NormalizeProviderName(provider)]
	if !ok {
		return nil
	}
	return append([]string(nil), p.AllowedRegions...)
}

func agentDBCreateRequestHandler(
	st *agentdb.Store,
	authority *agentDBLiveAuthority,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := readJSONMap(r)
		if err != nil {
			agentDBError(w, err)
			return
		}
		created, err := st.CreateRequest(r.Context(), requestCreateFromBody(r, m, authority))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, created)
	}
}
func agentDBListRequestsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.ListRequests(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"requests": rows})
	}
}
func agentDBGetRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, err := st.GetRequest(r.Context(), r.PathValue("request_id"))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}
func agentDBApproveRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		row, err := st.SetRequestDecision(r.Context(), r.PathValue("request_id"), agentdb.DecisionRequest{Decision: "approved", Reason: str(m, "reason")})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}
func agentDBDenyRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		row, err := st.SetRequestDecision(r.Context(), r.PathValue("request_id"), agentdb.DecisionRequest{Decision: "denied", Reason: str(m, "reason")})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}

func agentDBProvisionApprovedRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		dep, err := st.ProvisionApprovedRequest(
			r.Context(),
			r.PathValue("request_id"),
			agentdb.RequestProvisionRequest{
				DeploymentID:   str(m, "deployment_id"),
				LeaseSeconds:   integer(m, "lease_seconds"),
				Metadata:       obj(m, "metadata"),
				ProviderParams: obj(m, "provider_params"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, dep)
	}
}

// agentDBRestoreDrillHandler records an admin attestation of a completed
// restore drill with evidence; it is the only way to reach restore_verified
// (G8-B11). The attesting actor is the session user.
func agentDBRestoreDrillHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := requireActor(w, r)
		if !ok {
			return
		}
		m, err := readJSONMap(r)
		if err != nil {
			agentDBError(w, err)
			return
		}
		backup, err := st.RecordRestoreDrill(r.Context(), agentDBID(r),
			agentdb.RestoreDrillRequest{
				BackupID: str(m, "backup_id"), EvidenceURI: str(m, "evidence_uri"),
				Target: str(m, "target"), Checks: stringSlice(m, "checks"), ActorID: actor,
			})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, backup)
	}
}
