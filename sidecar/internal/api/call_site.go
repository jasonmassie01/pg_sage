package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The query page's call site (pr-01a): where in the application's
// repository a statement is sent from, read from the configured code host
// on request, so a slow host never delays the query page itself.

// callSiteTimeout bounds one lookup: a search and up to ten file reads.
const callSiteTimeout = 30 * time.Second

const callSiteOffNote = "No code host is configured: set agents.code_host.provider and " +
	"agents.code_host.repo, and the read token in SAGE_CODE_HOST_TOKEN."

// CallSiteSource is the code host and repository call-site lookups read;
// nil (or a nil Host) answers that lookups are off.
type CallSiteSource struct {
	Host codehost.Host
	Repo string
}

func (s *CallSiteSource) configured() bool { return s != nil && s.Host != nil }

func registerCallSiteRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	src *CallSiteSource) {
	routeAggregate(mux, "GET /api/v1/queries/{queryid}/call-site", gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler { return callSiteHandler(m, src) })
}

func callSiteHandler(mgr *fleet.DatabaseManager, src *CallSiteSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("queryid"), 10, 64)
		if err != nil {
			jsonError(w, "queryid must be a 64-bit integer", http.StatusBadRequest)
			return
		}
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" || database == "all" {
			jsonError(w, "database is required: a queryid belongs to one database",
				http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		body := map[string]any{"database": database, "queryid": strconv.FormatInt(id, 10),
			"configured": src.configured()}
		if !src.configured() {
			body["note"] = callSiteOffNote
			jsonResponse(w, body)
			return
		}
		sel, _ := resolveSingleDatabaseRequestPool(mgr, database)
		if sel.pool == nil {
			jsonError(w, "database "+database+" is not connected",
				http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), callSiteTimeout)
		defer cancel()
		site, err := agenttools.New(sel.pool, agenttools.Options{}).CallSite(ctx, src.Host,
			src.Repo, agenttools.QueryID(id))
		if err != nil {
			writeCallSiteError(w, r, err)
			return
		}
		body["repo"], body["host"] = src.Repo, src.Host.Name()
		body["call_site"], body["summary"] = site, site.Summary()
		jsonResponse(w, body)
	}
}

// writeCallSiteError maps a lookup error. Code-host errors carry no token
// (codehost redacts it), so their text is shown: it says what to fix. Any
// other error is a database one and stays in the server log.
func writeCallSiteError(w http.ResponseWriter, r *http.Request, err error) {
	var rl *codehost.RateLimitError
	switch {
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())))
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, agenttools.ErrNotFound):
		jsonError(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, agenttools.ErrInvalid):
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, agenttools.ErrUnavailable):
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
	case codeHostError(err):
		jsonError(w, err.Error(), http.StatusBadGateway)
	default:
		internalError(w, r, "find the call site", err)
	}
}

func codeHostError(err error) bool {
	for _, target := range []error{codehost.ErrForbidden, codehost.ErrNotFound,
		codehost.ErrUnavailable, codehost.ErrInvalid, egress.ErrBlocked,
		context.DeadlineExceeded} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
