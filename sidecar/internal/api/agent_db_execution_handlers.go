package api

import (
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBProvisionPreflightHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempt, err := st.PreflightProvision(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionExecuteHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempt, err := st.ExecuteProvision(
			r.Context(),
			agentDBID(r),
			agentdb.DryRunProvisionRunner{},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionStatusHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempt, err := st.CheckProvisionStatus(
			r.Context(),
			agentDBID(r),
			agentdb.DryRunProvisionRunner{},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionDestroyDryRunHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempt, err := st.DestroyProvisionDryRun(
			r.Context(),
			agentDBID(r),
			agentdb.DryRunProvisionRunner{},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionReconcileHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := st.ReconcileAbandonedDeployments(
			r.Context(),
			time.Now().UTC(),
			agentdb.DryRunProvisionRunner{},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, result)
	}
}

func agentDBBackupCheckHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		assurance, err := st.CheckBackupAssurance(
			r.Context(),
			agentDBID(r),
			agentdb.DryRunProvisionRunner{},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, assurance)
	}
}

func agentDBRestoreDrillDryRunHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempt, err := st.PlanRestoreDrillDryRun(
			r.Context(),
			agentDBID(r),
			agentdb.DryRunProvisionRunner{},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionAttemptsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempts, err := st.ProvisionAttempts(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"attempts": attempts})
	}
}
