package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// check_pressure over HTTP (guard-01a):
//
//	GET /api/v1/databases/{db}/pressure?table=&job=&batch=
//
// answers a backfill between batches: proceed, slow or stop, with every
// input's value, limits and freshness (internal/pressure). It only reads,
// cached for 2 s per table, and never acts. A signed-in role may call it,
// or instead a read-scope API token sent as `Authorization: Bearer`, by
// the migration lint route's pattern (migration_lint_token.go): the token
// is then the only credential, and a token limited to other databases
// gets 404 as for an unknown database. In a Databricks App the route is
// session-only.

const (
	pressurePath         = "/api/v1/databases/{db}/pressure"
	pressureDatabasesDir = "/api/v1/databases/"
	pressureCheckBudget  = 15 * time.Second
)

// isPressureTokenRequest reports a Bearer GET to the pressure route,
// which authenticates its token itself.
func isPressureTokenRequest(r *http.Request) bool {
	if _, bearer := bearerCredential(r); !bearer ||
		r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, pressureDatabasesDir)
	name, tail, found := strings.Cut(rest, "/")
	return ok && found && name != "" && tail == "pressure"
}

// registerPressureRoutes registers the pressure route; tokens validates
// API tokens (nil: a Bearer request is 503).
func registerPressureRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	tokens lintTokenValidator, appMode bool) {
	pattern := "GET " + pressurePath
	h := pressureHandler(mgr)
	if appMode {
		routeRead(mux, pattern, gateViewer, readDatabase(pathDB), h)
		return
	}
	session := RequireRole(viewerRoles...)(requireReadScope(readDatabase(pathDB))(h))
	registerRoute(mux, pattern, gateViewer, "",
		&readTokenOrSession{tokens: tokens, next: h, session: session})
	recordReadScope(mux, pattern, readDatabase(pathDB).String())
}

// readTokenOrSession serves a viewer GET of one path database: a Bearer
// request by its read-scope token alone, any other by a signed-in role.
type readTokenOrSession struct {
	tokens  lintTokenValidator
	next    http.Handler
	session http.Handler
}

func (h *readTokenOrSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secret, bearer := bearerCredential(r)
	if !bearer {
		h.session.ServeHTTP(w, r)
		return
	}
	p, ok := authenticateReadToken(w, r, h.tokens, secret)
	if !ok {
		return
	}
	p, err := scopeMCPPrincipal(r.Context(), databaseAuthzFrom(r.Context()), p)
	if err != nil {
		slog.Error("pressure route grant check failed; refusing the token request",
			"token_id", p.TokenID, "owner_user_id", p.OwnerUserID, "err", err)
		writeGrantStoreUnavailable(w)
		return
	}
	if !p.MayUseDatabase(r.PathValue("db")) {
		jsonError(w, "database not found", http.StatusNotFound)
		return
	}
	// Never both: drop any session user, so only the token acts.
	ctx := context.WithValue(r.Context(), userContextKey, (*auth.User)(nil))
	h.next.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(ctx, p)))
}

func pressureHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var inst *fleet.DatabaseInstance
		if mgr != nil {
			inst = mgr.GetInstance(r.PathValue("db"))
		}
		if inst == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		req, err := pressureRequest(r, inst.Name)
		if err != nil {
			sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), pressureCheckBudget)
		defer cancel()
		v, err := inst.Pressure.Check(ctx, req)
		if err != nil {
			writePressureError(w, inst.Name, req.Table, err)
			return
		}
		jsonResponse(w, v)
	}
}

func pressureRequest(r *http.Request, database string) (pressure.Request, error) {
	q := r.URL.Query()
	req := pressure.Request{Database: database, Table: q.Get("table"), Job: q.Get("job")}
	if raw := q.Get("batch"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return req, errors.New("batch must be a whole number >= 0")
		}
		req.Batch = n
	}
	return req, nil
}

func writePressureError(w http.ResponseWriter, database, table string, err error) {
	switch {
	case errors.Is(err, pressure.ErrUnknownTable):
		sreErrorCode(w, "table "+strconv.Quote(table)+" not found in "+database+
			" (it must resolve with to_regclass, schema-qualified when not on the "+
			"search path)", "table_not_found", http.StatusNotFound)
	case errors.Is(err, pressure.ErrInvalidRequest):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, pressure.ErrUnavailable):
		sreErrorCode(w, "the pressure check of "+database+" is unavailable: the database "+
			"is not connected", "pressure_unavailable", http.StatusServiceUnavailable)
	default:
		slog.Error("pressure check failed", "database", database, "error", err)
		w.Header().Set("Retry-After", "2")
		sreErrorCode(w, "the pressure inputs of "+database+" could not be read; retry, "+
			"and treat no answer as stop", "pressure_unreadable",
			http.StatusServiceUnavailable)
	}
}

// PressureMCP serves MCP check_pressure with the route's check.
func PressureMCP(mgr *fleet.DatabaseManager) mcp.PressureBackend {
	return pressureMCP{mgr: mgr}
}

type pressureMCP struct{ mgr *fleet.DatabaseManager }

func (b pressureMCP) CheckPressure(ctx context.Context, req mcp.PressureRequest) (any,
	error) {
	var inst *fleet.DatabaseInstance
	if b.mgr != nil {
		inst = b.mgr.GetInstance(req.Database)
	}
	if inst == nil {
		return nil, mcp.ErrPressureDatabaseNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, pressureCheckBudget)
	defer cancel()
	v, err := inst.Pressure.Check(ctx, pressure.Request{Database: inst.Name,
		Table: req.Table, Job: req.Job, Batch: req.Batch})
	if err != nil {
		return nil, err
	}
	return v, nil
}
