package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBDeployRequestsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.DeployRequests(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"deploy_requests": rows})
	}
}

func agentDBGetDeployRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, err := st.GetDeployRequest(
			r.Context(),
			agentDBID(r),
			r.PathValue("deploy_request_id"),
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}

func agentDBCreateDeployRequestHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		row, err := st.CreateDeployRequest(
			r.Context(),
			agentDBID(r),
			agentdb.DeployRequestCreate{
				DeployRequestID:    str(m, "deploy_request_id"),
				TargetDatabaseName: str(m, "target_database_name"),
				TargetSchemaName:   str(m, "target_schema_name"),
				Title:              str(m, "title"),
				Reason:             str(m, "reason"),
				Status:             str(m, "status"),
				RiskTier:           str(m, "risk_tier"),
				MigrationSQL:       str(m, "migration_sql"),
				VerificationSQL:    str(m, "verification_sql"),
				RollbackSQL:        str(m, "rollback_sql"),
				ForwardFixNotes:    str(m, "forward_fix_notes"),
				GateResults:        obj(m, "gate_results"),
				CreatedBy:          str(m, "created_by"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}

func agentDBRequestDeployReviewHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, err := st.RequestDeployReview(
			r.Context(),
			agentDBID(r),
			r.PathValue("deploy_request_id"),
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}

func agentDBReviewDeployRequestHandler(
	st *agentdb.Store,
	decision string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		row, err := st.ReviewDeployRequest(
			r.Context(),
			agentDBID(r),
			r.PathValue("deploy_request_id"),
			agentdb.DeployRequestReview{
				Decision:     decision,
				ReviewedBy:   str(m, "reviewed_by"),
				ReviewReason: str(m, "review_reason"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}
