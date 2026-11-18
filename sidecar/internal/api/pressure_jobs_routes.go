package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// Backfill job registration (guard-01b):
//
//	POST /api/v1/databases/{db}/pressure-jobs            register (or refresh)
//	GET  /api/v1/databases/{db}/pressure-jobs            list the active jobs
//	POST /api/v1/databases/{db}/pressure-jobs/{id}/end   end a job early
//
// A job names the application_name its sessions run under; with enforce,
// pg_sage may propose cancelling its writing backend after two consecutive
// stop verdicts, always for a person to approve. Registering and ending
// take an operator session or, outside a Databricks App, an operator API
// token with the propose scope (plan_token.go); listing takes a viewer
// session or a read-scope token. A token limited to other databases gets
// 404 as for an unknown database.

const (
	pressureJobsPath    = "/api/v1/databases/{db}/pressure-jobs"
	pressureJobEndPath  = "/api/v1/databases/{db}/pressure-jobs/{id}/end"
	maxPressureJobBody  = 4 << 10
	pressureJobsTimeout = 10 * time.Second
)

// isPressureJobTokenRequest reports a Bearer request to a pressure-jobs
// route, which authenticates its token itself.
func isPressureJobTokenRequest(r *http.Request) bool {
	if _, bearer := bearerCredential(r); !bearer {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, pressureDatabasesDir)
	name, tail, found := strings.Cut(rest, "/")
	if !ok || !found || name == "" {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return tail == "pressure-jobs"
	case http.MethodPost:
		id, end, isEnd := strings.Cut(strings.TrimPrefix(tail, "pressure-jobs/"), "/")
		return tail == "pressure-jobs" ||
			strings.HasPrefix(tail, "pressure-jobs/") && isEnd && id != "" && end == "end"
	}
	return false
}

// registerPressureJobRoutes registers the job routes; tokens validates API
// tokens (nil: a Bearer request is 503).
func registerPressureJobRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	tokens lintTokenValidator, appMode bool) {
	h := pressureJobHandlers{mgr: mgr}
	list := "GET " + pressureJobsPath
	if appMode {
		routeRead(mux, list, gateViewer, readDatabase(pathDB), http.HandlerFunc(h.list))
	} else {
		session := RequireRole(viewerRoles...)(requireReadScope(readDatabase(pathDB))(
			http.HandlerFunc(h.list)))
		registerRoute(mux, list, gateViewer, "", &readTokenOrSession{tokens: tokens,
			next: http.HandlerFunc(h.list), session: session})
		recordReadScope(mux, list, readDatabase(pathDB).String())
	}
	for _, d := range []struct {
		pattern string
		decide  http.HandlerFunc
	}{{"POST " + pressureJobsPath, h.register}, {"POST " + pressureJobEndPath, h.end}} {
		inner := requireDatabaseRole(auth.RoleOperator, pathDB)(d.decide)
		registerPlanRoute(mux, d.pattern, gateOperator, "database:"+pathDB.name, inner, tokens,
			appMode, mcptoken.ScopePropose)
	}
}

type pressureJobHandlers struct{ mgr *fleet.DatabaseManager }

// registry is the path database's job store, answering the refusal itself.
func (h pressureJobHandlers) registry(w http.ResponseWriter,
	r *http.Request) (string, pressure.JobRegistry, bool) {
	name := r.PathValue("db")
	var inst *fleet.DatabaseInstance
	if h.mgr != nil && tokenMayUse(r, name) {
		inst = h.mgr.GetInstance(name)
	}
	switch {
	case inst == nil:
		jsonError(w, "database not found", http.StatusNotFound)
		return "", nil, false
	case inst.PressureJobs == nil:
		sreErrorCode(w, "pressure jobs of "+name+" are unavailable: the database is not "+
			"connected", "pressure_jobs_unavailable", http.StatusServiceUnavailable)
		return "", nil, false
	}
	return inst.Name, inst.PressureJobs, true
}

// pressureJobRequest is a registration body.
type pressureJobRequest struct {
	ApplicationName string `json:"application_name"`
	Table           string `json:"table"`
	Enforce         bool   `json:"enforce"`
	TTLSeconds      int64  `json:"ttl_seconds"`
}

func (h pressureJobHandlers) register(w http.ResponseWriter, r *http.Request) {
	db, reg, ok := h.registry(w, r)
	if !ok {
		return
	}
	var body pressureJobRequest
	if err := decodeOneJSON(r, &body); err != nil {
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
		return
	}
	if body.TTLSeconds < 0 || body.TTLSeconds > int64(pressure.MaxJobTTL/time.Second) {
		sreErrorCode(w, fmt.Sprintf("ttl_seconds must be 0 (the default, %s) to %d",
			pressure.DefaultJobTTL, int64(pressure.MaxJobTTL/time.Second)),
			"invalid_request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pressureJobsTimeout)
	defer cancel()
	j, err := reg.Register(ctx, pressure.JobSpec{Database: db,
		ApplicationName: body.ApplicationName, Table: body.Table, Enforce: body.Enforce,
		TTL: time.Duration(body.TTLSeconds) * time.Second, RegisteredBy: registrant(r)})
	if err != nil {
		writePressureJobError(w, db, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, j)
}

func (h pressureJobHandlers) list(w http.ResponseWriter, r *http.Request) {
	db, reg, ok := h.registry(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pressureJobsTimeout)
	defer cancel()
	jobs, err := reg.Active(ctx, db)
	if err != nil {
		writePressureJobError(w, db, err)
		return
	}
	if jobs == nil {
		jobs = []pressure.Job{}
	}
	jsonResponse(w, map[string]any{"jobs": jobs})
}

func (h pressureJobHandlers) end(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		sreErrorCode(w, "job id must be a positive integer", "invalid_request",
			http.StatusBadRequest)
		return
	}
	db, reg, ok := h.registry(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pressureJobsTimeout)
	defer cancel()
	j, err := reg.End(ctx, db, id)
	if err != nil {
		writePressureJobError(w, db, err)
		return
	}
	jsonResponse(w, j)
}

// registrant names who registered: the API token, else the signed-in user.
func registrant(r *http.Request) string {
	if p, ok := mcp.PrincipalFromContext(r.Context()); ok && p.TokenID != "" {
		return "token:" + p.TokenID
	}
	if u := UserFromContext(r.Context()); u != nil {
		return "user:" + strconv.Itoa(u.ID)
	}
	return "unknown"
}

// decodeOneJSON reads exactly one JSON object with known fields.
func decodeOneJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPressureJobBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	if dec.More() {
		return errors.New("invalid JSON body: one object expected")
	}
	return nil
}

func writePressureJobError(w http.ResponseWriter, database string, err error) {
	switch {
	case errors.Is(err, pressure.ErrInvalidRequest):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, pressure.ErrJobNotFound):
		sreErrorCode(w, "no active pressure job with that id in "+database, "job_not_found",
			http.StatusNotFound)
	default:
		slog.Error("pressure job store failed", "database", database, "error", err)
		w.Header().Set("Retry-After", "5")
		sreErrorCode(w, "the pressure jobs of "+database+" could not be read or written; "+
			"retry", "pressure_jobs_unavailable", http.StatusServiceUnavailable)
	}
}
