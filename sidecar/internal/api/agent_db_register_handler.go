package api

import (
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// agentDBRegisterHandler registers or provisions a deployment directly.
//
// D4: a cloud provider deployment spends money once live, and approval is
// the evidence that earns that spend. Cloud registers therefore require an
// approved, unused request_id and consume it exactly once; without one they
// are rejected with 409. local_postgres schema/database registers keep the
// direct path (their DDL is separately gated by
// PG_SAGE_AGENTDB_LOCAL_PROVISIONING). Any register that names a request_id
// goes through the same single-use consumption.
func agentDBRegisterHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		if requestID := strings.TrimSpace(str(m, "request_id")); requestID != "" {
			actor, ok := requireActor(w, r)
			if !ok {
				return
			}
			provisionFromRequest(w, r, st, requestID, requestProvisionFromBody(m, actor))
			return
		}
		if agentdb.IsCloudProvider(str(m, "provider")) {
			jsonError(w, "approved request required: create a request and provision "+
				"it with POST /api/v1/agent-dbs/requests/{id}/provision "+
				"or pass request_id", http.StatusConflict)
			return
		}
		d, err := st.Provision(r.Context(), registerFromBody(m))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}

func registerFromBody(m map[string]any) agentdb.RegisterRequest {
	return agentdb.RegisterRequest{
		DeploymentID:  str(m, "deployment_id"),
		TenantID:      str(m, "tenant_id"),
		AgentID:       str(m, "agent_id"),
		RunID:         str(m, "run_id"),
		DatabaseName:  str(m, "database_name"),
		SafetyMode:    str(m, "safety_mode"),
		IsolationType: str(m, "isolation_type"),
		Provider:      str(m, "provider"),
		ProvisioningLevel: firstString(
			str(m, "provisioning_level"), str(m, "isolation_type"),
		),
		SizeProfileID: str(m, "size_profile_id"),
		SchemaName:    str(m, "schema_name"),
		// G8-B14: validated against the env allow-list by the store.
		SecretRef:         str(m, "secret_ref"),
		SecretRefProvider: str(m, "secret_ref_provider"),
		LeaseSeconds:      integer(m, "lease_seconds"),
		BudgetUSD:         float(m, "budget_usd"),
		BackupRequired:    boolValue(m, "backup_required"),
		Metadata:          obj(m, "metadata"),
	}
}
