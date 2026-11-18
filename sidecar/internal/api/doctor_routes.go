package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/doctor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
)

// doctorModelNotRequested is the model check's detail on a plain GET: the
// probe is a real, budget-charged request, so the page asks for it.
const doctorModelNotRequested = "not probed: add ?probe_model=true to send one tiny " +
	"request to the model (charged to the token budget)"

// doctorDeps are what GET /api/v1/doctor checks: the monitored database
// from the fleet, the meta database and the model prober.
type doctorDeps struct {
	mgr   *fleet.DatabaseManager
	meta  *pgxpool.Pool
	model LLMProber
}

// registerDoctorRoute adds the admin-only doctor report (ux-04a) that the
// setup wizard (ux-04b) reads. It never sends a test notification.
func registerDoctorRoute(mux *http.ServeMux, mgr *fleet.DatabaseManager, cfg *config.Config,
	control *pgxpool.Pool, budgets LLMBudgetRegistry) {
	d := doctorDeps{mgr: mgr}
	if cfg != nil && cfg.HasMetaDB() {
		d.meta = control
	}
	if prober, ok := budgets.(LLMProber); ok {
		d.model = prober
	}
	route(mux, "GET /api/v1/doctor", gateAdmin, doctorHandler(d))
}

// doctorHandler runs doctor against ?database= (or the only database).
func doctorHandler(d doctorDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("database")
		if err := validateDatabaseParam(name); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		inst, status, msg := doctorInstance(d.mgr, name)
		if inst == nil {
			jsonError(w, msg, status)
			return
		}
		in := doctor.Inputs{Target: inst.Pool, Meta: d.meta,
			ExpectedSteps: schema.MigrationStepNames(), ModelSkipped: doctorModelNotRequested}
		if r.URL.Query().Get("probe_model") == "true" {
			in.ModelSkipped = ""
			if d.model != nil {
				in.Model = func(ctx context.Context) llm.ProbeResult {
					return d.model.ProbeLLM(ctx)
				}
			}
		}
		jsonResponse(w, doctor.Run(r.Context(), in))
	}
}

// doctorInstance picks the database: the named one, or the only one.
func doctorInstance(mgr *fleet.DatabaseManager, name string) (*fleet.DatabaseInstance,
	int, string) {
	var instances map[string]*fleet.DatabaseInstance
	if mgr != nil {
		instances = mgr.Instances()
	}
	if len(instances) == 0 {
		return nil, http.StatusNotFound, "no monitored database"
	}
	if name == "" || name == "all" {
		if len(instances) > 1 {
			return nil, http.StatusBadRequest, "name one database: ?database=<name>"
		}
		for _, inst := range instances {
			return inst, 0, ""
		}
	}
	if inst := instances[name]; inst != nil {
		return inst, 0, ""
	}
	return nil, http.StatusNotFound, "database not found"
}
