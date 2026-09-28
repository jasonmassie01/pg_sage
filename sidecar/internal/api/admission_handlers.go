package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/verify"
)

// admissionListHandler reports each database's autonomous index-build
// load-admission status (D6), optionally filtered by ?database=.
func admissionListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		statuses := []executor.IndexAdmissionStatus{}
		if mgr != nil {
			for name, inst := range mgr.Instances() {
				if database == "" || database == "all" || database == name {
					statuses = append(statuses, instanceAdmission(r.Context(), name, inst))
				}
			}
		}
		sort.Slice(statuses, func(i, j int) bool {
			return statuses[i].Database < statuses[j].Database
		})
		jsonResponse(w, map[string]any{"databases": statuses})
	}
}

// admissionStatusHandler reports one database's load-admission status.
func admissionStatusHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := validateDatabaseParam(name); err != nil || name == "" {
			jsonError(w, errInvalidDatabaseParam.Error(), http.StatusBadRequest)
			return
		}
		if mgr == nil || mgr.GetInstance(name) == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, instanceAdmission(r.Context(), name, mgr.GetInstance(name)))
	}
}

func instanceAdmission(
	ctx context.Context, name string, inst *fleet.DatabaseInstance,
) executor.IndexAdmissionStatus {
	if inst == nil || inst.Executor == nil {
		return executor.IndexAdmissionStatus{
			Database: name, Reason: "executor_unavailable",
			Mode: verify.EvidenceUnavailable, MissingEvidence: []string{"executor"},
			Detail: "no running executor for this database", CheckedAt: time.Now().UTC(),
		}
	}
	status := inst.Executor.IndexAdmissionStatus(ctx)
	status.Database = name
	return status
}
