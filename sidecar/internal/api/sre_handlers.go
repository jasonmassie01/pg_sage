package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE investigation routes (AI-SRE-SPEC §9, Codex §6). Reads are
// open to every signed-in role; export, pinning, start, stop and resume
// need an operator (CHECK-25). {db} is the fleet database name; every
// lookup is scoped to that database's bound identity, so an id of another
// database is not found (CHECK-09/26). Nothing here executes anything.

const sreInvestigationsPath = "/api/v1/databases/{db}/investigations"

// fleetInvestigationLimit bounds each database's page in the fleet list.
const fleetInvestigationLimit = 100

func registerSRERoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewerUp := RequireRole("admin", "operator", "viewer")
	mux.Handle("GET /api/v1/investigations", viewerUp(fleetInvestigationsHandler(mgr)))
	perDB := perDatabaseSREMux(mgr)
	// The per-database routes go through their own mux behind one
	// catch-all per method: "{db}/investigations" and the fleet-mode
	// "managed/{id}" routes overlap with neither more specific, which
	// ServeMux refuses at registration. The managed routes stay more
	// specific than the catch-all and keep their paths. The bare
	// "{db}" patterns stop ServeMux redirecting "/databases/x" to the
	// catch-all's "/databases/x/"; the inner mux answers them 404.
	for _, method := range []string{"GET", "POST"} {
		mux.Handle(method+" /api/v1/databases/{db}", perDB)
		mux.Handle(method+" /api/v1/databases/{db}/{rest...}", perDB)
	}
}

func perDatabaseSREMux(mgr *fleet.DatabaseManager) *http.ServeMux {
	viewerUp := RequireRole("admin", "operator", "viewer")
	operatorUp := RequireRole("admin", "operator")
	base := sreInvestigationsPath
	mux := http.NewServeMux()
	mux.Handle("GET "+base, viewerUp(investigationListHandler(mgr)))
	mux.Handle("GET "+base+"/{id}", viewerUp(investigationDetailHandler(mgr)))
	mux.Handle("GET "+base+"/{id}/events", viewerUp(investigationEventsHandler(mgr)))
	mux.Handle("GET "+base+"/{id}/evidence/{evidence}",
		viewerUp(investigationEvidenceHandler(mgr)))
	mux.Handle("GET "+base+"/{id}/export", operatorUp(investigationExportHandler(mgr)))
	mux.Handle("POST "+base+"/{id}/pin", operatorUp(investigationPinHandler(mgr, true)))
	mux.Handle("POST "+base+"/{id}/unpin", operatorUp(investigationPinHandler(mgr, false)))
	mux.Handle("POST "+base, operatorUp(investigationStartHandler(mgr)))
	mux.Handle("POST "+base+"/{id}/stop", operatorUp(investigationTransitionHandler(mgr,
		false)))
	mux.Handle("POST "+base+"/{id}/resume", operatorUp(investigationTransitionHandler(mgr,
		true)))
	registerSREActionRoutes(mux, mgr)
	return mux
}

// sreErrorResponse writes a canonical error code with its status.
func sreErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sre.ErrNotFound):
		sreErrorCode(w, "investigation not found", "not_found", http.StatusNotFound)
	case errors.Is(err, sre.ErrInvalidRequest):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, sre.ErrMetadataUnavailable):
		sreErrorCode(w, "investigation store unavailable", "metadata_unavailable",
			http.StatusServiceUnavailable)
	default:
		internalError(w, r, "sre investigations", err)
	}
}

func sreErrorCode(w http.ResponseWriter, msg, code string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// investigationService resolves {db} to its investigator.
func investigationService(
	w http.ResponseWriter, mgr *fleet.DatabaseManager, name string,
) (*sre.Service, bool) {
	if name == "" || name == "all" || validateDatabaseParam(name) != nil {
		sreErrorCode(w, "invalid database name", "invalid_request", http.StatusBadRequest)
		return nil, false
	}
	var inst *fleet.DatabaseInstance
	if mgr != nil {
		inst = mgr.GetInstance(name)
	}
	if inst == nil {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return nil, false
	}
	if inst.Investigations == nil {
		sreErrorCode(w, "investigations unavailable for this database",
			"metadata_unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return inst.Investigations, true
}

func listFilter(r *http.Request) (sre.ListFilter, error) {
	q := r.URL.Query()
	f := sre.ListFilter{Cursor: q.Get("cursor"), CaseID: q.Get("case_id")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return f, fmt.Errorf("%w: limit must be a positive integer",
				sre.ErrInvalidRequest)
		}
		f.Limit = n
	}
	return f, nil
}

func investigationListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		f, err := listFilter(r)
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		page, err := svc.List(r.Context(), f)
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"database": svc.Name(), "items": page.Items,
			"next_cursor": page.NextCursor})
	}
}

func investigationDetailHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		d, err := svc.Detail(r.Context(), sre.UUID(r.PathValue("id")))
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		jsonResponse(w, d)
	}
}

func investigationEventsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		events, verified, err := svc.Events(r.Context(), sre.UUID(r.PathValue("id")))
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"events": events, "chain_verified": verified})
	}
}

func investigationEvidenceHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		ev, err := svc.EvidenceItem(r.Context(), sre.UUID(r.PathValue("id")),
			sre.UUID(r.PathValue("evidence")))
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		jsonResponse(w, ev)
	}
}

func investigationExportHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		id := sre.UUID(r.PathValue("id"))
		switch r.URL.Query().Get("format") {
		case "", "json":
			doc, err := svc.Export(r.Context(), id)
			if err != nil {
				sreErrorResponse(w, r, err)
				return
			}
			w.Header().Set("Content-Disposition", fmt.Sprintf(
				`attachment; filename="investigation-%s.json"`, doc.Investigation.ID))
			jsonResponse(w, doc)
		case "markdown":
			md, err := svc.ExportMarkdown(r.Context(), id)
			if err != nil {
				sreErrorResponse(w, r, err)
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte(md))
		default:
			sreErrorCode(w, "format must be json or markdown", "invalid_request",
				http.StatusBadRequest)
		}
	}
}

func investigationPinHandler(mgr *fleet.DatabaseManager, pinned bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		actor := fmt.Sprintf("user:%d", UserFromContext(r.Context()).ID)
		inv, err := svc.SetPinned(r.Context(), sre.UUID(r.PathValue("id")), pinned, actor)
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		jsonResponse(w, inv)
	}
}

// fleetInvestigationsHandler lists the latest investigations of one or
// every database, each tagged with its database, for the Cases panel.
// A database whose investigator cannot be read is named in unavailable.
func fleetInvestigationsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, database) {
			return
		}
		f, err := listFilter(r)
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		if f.Limit == 0 {
			f.Limit = fleetInvestigationLimit
		}
		services, unavailable := investigationServices(mgr, database)
		items, failed, err := sre.ListAcross(r.Context(), services, f)
		if err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"database": responseDatabaseName(database),
			"items": items, "unavailable": append(unavailable, failed...)})
	}
}

// investigationServices returns the selected databases' investigators and
// the names of those without one.
func investigationServices(mgr *fleet.DatabaseManager,
	database string) ([]*sre.Service, []string) {
	if mgr == nil {
		return nil, []string{}
	}
	var instances []*fleet.DatabaseInstance
	if database != "" && database != "all" {
		instances = []*fleet.DatabaseInstance{mgr.GetInstance(database)}
	} else {
		for _, inst := range mgr.Instances() {
			instances = append(instances, inst)
		}
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })
	services, missing := []*sre.Service{}, []string{}
	for _, inst := range instances {
		if inst.Investigations == nil {
			missing = append(missing, inst.Name)
			continue
		}
		services = append(services, inst.Investigations)
	}
	return services, missing
}
