package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func registerAgentDBRoutes(mux *http.ServeMux, st *agentdb.Store) {
	operatorUp := RequireRole("admin", "operator")
	mux.Handle(
		"POST /api/v1/agent-dbs/{deployment_id}/agent-ping",
		http.HandlerFunc(agentDBTokenPingHandler(st)),
	)
	mux.Handle("GET /api/v1/agent-dbs", operatorUp(http.HandlerFunc(agentDBListHandler(st))))
	mux.Handle("POST /api/v1/agent-dbs", operatorUp(http.HandlerFunc(agentDBRegisterHandler(st))))
	mux.Handle("POST /api/v1/agent-dbs/cleanup", operatorUp(http.HandlerFunc(agentDBCleanupAllHandler(st))))
	mux.Handle("/api/v1/agent-dbs/", operatorUp(http.HandlerFunc(agentDBSubrouter(st))))
}

func agentDBSubrouter(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/agent-dbs/")
		parts := strings.Split(rest, "/")
		if len(parts) == 0 || parts[0] == "" {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		if parts[0] == "requests" {
			if len(parts) >= 2 {
				r.SetPathValue("request_id", parts[1])
			}
			switch {
			case r.Method == http.MethodPost && len(parts) == 1:
				agentDBCreateRequestHandler(st)(w, r)
				return
			case r.Method == http.MethodGet && len(parts) == 1:
				agentDBListRequestsHandler(st)(w, r)
				return
			case r.Method == http.MethodGet && len(parts) == 2:
				agentDBGetRequestHandler(st)(w, r)
				return
			case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "approve":
				agentDBApproveRequestHandler(st)(w, r)
				return
			case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "deny":
				agentDBDenyRequestHandler(st)(w, r)
				return
			}
		}
		if parts[0] == "providers" && r.Method == http.MethodGet && len(parts) == 1 {
			agentDBProvidersHandler()(w, r)
			return
		}
		if parts[0] == "identities" {
			switch {
			case r.Method == http.MethodGet && len(parts) == 1:
				agentDBIdentitiesHandler(st)(w, r)
				return
			case r.Method == http.MethodPost && len(parts) == 1:
				agentDBUpsertIdentityHandler(st)(w, r)
				return
			}
		}
		if parts[0] == "reconcile" && r.Method == http.MethodPost && len(parts) == 1 {
			agentDBProvisionReconcileHandler(st)(w, r)
			return
		}
		if parts[0] == "size-profiles" {
			if len(parts) >= 2 {
				r.SetPathValue("profile_id", parts[1])
			}
			switch {
			case r.Method == http.MethodGet && len(parts) == 1:
				agentDBListSizeProfilesHandler(st)(w, r)
				return
			case r.Method == http.MethodPost && len(parts) == 1:
				agentDBUpsertSizeProfileHandler(st)(w, r)
				return
			case r.Method == http.MethodDelete && len(parts) == 2:
				agentDBDeleteSizeProfileHandler(st)(w, r)
				return
			}
		}
		r.SetPathValue("deployment_id", parts[0])
		switch {
		case r.Method == http.MethodGet && len(parts) == 1:
			agentDBGetHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "ping":
			agentDBPingHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "extend-lease":
			agentDBLeaseHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "recommendations":
			agentDBRecommendationsHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "recommendations":
			agentDBCreateRecommendationHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 4 && parts[1] == "recommendations" && parts[3] == "feedback":
			r.SetPathValue("recommendation_id", parts[2])
			agentDBFeedbackHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "audit":
			agentDBAuditHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "audit" && parts[2] == "export":
			agentDBAuditExportHandler(st)(w, r)
		case len(parts) >= 2 && parts[1] == "deploy-requests":
			if len(parts) >= 3 {
				r.SetPathValue("deploy_request_id", parts[2])
			}
			switch {
			case r.Method == http.MethodGet && len(parts) == 2:
				agentDBDeployRequestsHandler(st)(w, r)
			case r.Method == http.MethodPost && len(parts) == 2:
				agentDBCreateDeployRequestHandler(st)(w, r)
			case r.Method == http.MethodGet && len(parts) == 3:
				agentDBGetDeployRequestHandler(st)(w, r)
			case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "request-review":
				agentDBRequestDeployReviewHandler(st)(w, r)
			case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "approve":
				agentDBReviewDeployRequestHandler(st, "approved")(w, r)
			case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "deny":
				agentDBReviewDeployRequestHandler(st, "denied")(w, r)
			default:
				jsonError(w, "not found", http.StatusNotFound)
			}
		case len(parts) >= 2 && parts[1] == "ping-tokens":
			if len(parts) >= 3 {
				r.SetPathValue("token_id", parts[2])
			}
			switch {
			case r.Method == http.MethodGet && len(parts) == 2:
				agentDBPingTokensHandler(st)(w, r)
			case r.Method == http.MethodPost && len(parts) == 2:
				agentDBCreatePingTokenHandler(st)(w, r)
			case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "rotate":
				agentDBRotatePingTokenHandler(st)(w, r)
			case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "revoke":
				agentDBRevokePingTokenHandler(st)(w, r)
			default:
				jsonError(w, "not found", http.StatusNotFound)
			}
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "agent-ping":
			agentDBTokenPingHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "cost-samples":
			agentDBCostSampleHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "cost":
			agentDBCostHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "backups":
			agentDBBackupsHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "backups":
			agentDBRecordBackupHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "backups" && parts[2] == "check":
			agentDBBackupCheckHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "backups" && parts[2] == "restore-drill-dry-run":
			agentDBRestoreDrillDryRunHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "tuning-hints":
			agentDBTuningHintsHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "provision" && parts[2] == "preflight":
			agentDBProvisionPreflightHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "provision" && parts[2] == "execute":
			agentDBProvisionExecuteHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "provision" && parts[2] == "status":
			agentDBProvisionStatusHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "provision" && parts[2] == "destroy-dry-run":
			agentDBProvisionDestroyDryRunHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "provision" && parts[2] == "attempts":
			agentDBProvisionAttemptsHandler(st)(w, r)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "cleanup":
			agentDBCleanupHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "archive":
			agentDBArchiveHandler(st)(w, r)
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "restore":
			agentDBRestoreHandler(st)(w, r)
		case r.Method == http.MethodDelete && len(parts) == 1:
			agentDBDeleteHandler(st)(w, r)
		default:
			jsonError(w, "not found", http.StatusNotFound)
		}
	}
}

func agentDBCreateRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		created, err := st.CreateRequest(r.Context(), agentdb.RequestCreate{
			RequestID:          str(m, "request_id"),
			TenantID:           str(m, "tenant_id"),
			AgentID:            str(m, "agent_id"),
			OwnerID:            str(m, "owner_id"),
			RunID:              str(m, "run_id"),
			Purpose:            str(m, "purpose"),
			IsolationType:      str(m, "requested_isolation_type"),
			DatabaseName:       str(m, "database_name"),
			Provider:           str(m, "provider"),
			IdempotencyKey:     firstString(r.Header.Get("Idempotency-Key"), str(m, "idempotency_key")),
			BudgetUSD:          float(m, "budget_usd"),
			BackupRequired:     boolValue(m, "backup_required"),
			DataClassification: str(m, "data_classification"),
			MaskingPolicyID:    str(m, "masking_policy_id"),
			Region:             str(m, "region"),
			AllowedRegions:     stringSlice(m, "allowed_regions"),
			ApprovalSLASeconds: integer(m, "approval_sla_seconds"),
			Body:               m,
		})
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
func agentDBListHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.List(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"deployments": rows})
	}
}
func agentDBCleanupAllHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.ArchiveExpired(r.Context(), time.Now().UTC())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"archived": rows})
	}
}
func agentDBID(r *http.Request) string { return r.PathValue("deployment_id") }
func agentDBError(w http.ResponseWriter, err error) {
	if errors.Is(err, agentdb.ErrNotFound) {
		jsonError(w, "agent db deployment not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, agentdb.ErrRestoreRequired) {
		jsonError(w, "verified restore required", http.StatusConflict)
		return
	}
	if errors.Is(err, agentdb.ErrRateLimited) {
		jsonError(w, "agent db rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if errors.Is(err, agentdb.ErrInvalid) {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if errors.Is(err, agentdb.ErrConflict) {
		jsonError(w, "idempotency conflict", http.StatusConflict)
		return
	}
	jsonError(w, "agent db store error", http.StatusInternalServerError)
}
func readMap(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}
func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func integer(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		i, _ := strconv.Atoi(v)
		return i
	}
	return 0
}
func obj(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return nil
}

func stringSlice(m map[string]any, k string) []string {
	switch v := m[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func agentDBRegisterHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		req := agentdb.RegisterRequest{
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
			SizeProfileID:  str(m, "size_profile_id"),
			SchemaName:     str(m, "schema_name"),
			LeaseSeconds:   integer(m, "lease_seconds"),
			BudgetUSD:      float(m, "budget_usd"),
			BackupRequired: boolValue(m, "backup_required"),
			Metadata:       obj(m, "metadata"),
		}
		d, err := st.Provision(r.Context(), req)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}

func agentDBGetHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}
func agentDBPingHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		d, err := st.Ping(r.Context(), agentDBID(r), agentdb.PingRequest{Status: str(m, "status"), Metrics: obj(m, "metrics")})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}
func agentDBLeaseHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		d, err := st.ExtendLease(r.Context(), agentDBID(r), agentdb.LeaseRequest{LeaseSeconds: integer(m, "lease_seconds"), Reason: str(m, "reason")})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}
func agentDBRecommendationsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recs, err := st.Recommendations(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"recommendations": recs})
	}
}
func agentDBCreateRecommendationHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		rec, err := st.UpsertRecommendation(r.Context(), agentDBID(r), agentdb.RecommendationCreate{
			RecommendationID:  str(m, "recommendation_id"),
			Kind:              str(m, "kind"),
			Title:             str(m, "title"),
			Detail:            str(m, "detail"),
			QueryFingerprint:  str(m, "query_fingerprint"),
			ActionType:        str(m, "action_type"),
			ActionRisk:        str(m, "action_risk"),
			Confidence:        float(m, "confidence"),
			AgentInstructions: obj(m, "agent_instructions"),
			Payload:           obj(m, "payload"),
		})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, rec)
	}
}
func agentDBFeedbackHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		err := st.Feedback(
			r.Context(),
			agentDBID(r),
			r.PathValue("recommendation_id"),
			agentdb.FeedbackRequest{
				Decision: str(m, "decision"),
				Comment:  str(m, "comment"),
				Applied:  boolValue(m, "applied"),
				Result:   str(m, "result"),
				Error:    str(m, "error"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]bool{"ok": true})
	}
}
func agentDBCostSampleHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		err := st.AddCostSample(r.Context(), agentDBID(r), agentdb.CostSampleRequest{CostUSD: float(m, "cost_usd"), Metric: str(m, "metric"), Value: float(m, "value"), Unit: str(m, "unit"), Detail: obj(m, "detail")})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]bool{"ok": true})
	}
}
func agentDBCostHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cost, err := st.Cost(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"cost": cost})
	}
}
func agentDBBackupsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		backups, err := st.Backups(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"backups": backups})
	}
}
func agentDBRecordBackupHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		backup, err := st.RecordBackup(r.Context(), agentDBID(r), agentdb.BackupRequest{
			BackupID:   str(m, "backup_id"),
			Provider:   str(m, "provider"),
			Status:     str(m, "status"),
			ArchiveURI: str(m, "archive_uri"),
			Detail:     obj(m, "detail"),
		})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, backup)
	}
}
func agentDBTuningHintsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hints, err := st.TuningHints(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"tuning_hints": hints})
	}
}
func agentDBCleanupHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decision, err := st.CleanupDecision(
			r.Context(), agentDBID(r), time.Now().UTC(),
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"cleanup": decision})
	}
}
func float(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}
func boolValue(m map[string]any, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}
func firstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func agentDBArchiveHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := st.Archive(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}
func agentDBRestoreHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := st.Restore(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, d)
	}
}
func agentDBDeleteHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := st.Delete(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]bool{"deleted": true})
	}
}
