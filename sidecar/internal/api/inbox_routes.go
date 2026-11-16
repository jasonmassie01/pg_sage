package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// The inbox (ux-01a): GET /api/v1/inbox lists every decision waiting on a
// person across the databases the caller may read (?database=, ?kinds=,
// ?limit=); GET /api/v1/inbox/count answers the counts only. Both are
// read-only: an item is decided on its own page (Href), through its own
// route. A source that could not be read is named in errors and the total
// is then a lower bound. Viewers see the items with actionable=false.

const inboxPath = "/api/v1/inbox"

func registerInboxRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	ledgers *earned.Registry) {
	for _, countOnly := range []bool{false, true} {
		pattern := "GET " + inboxPath
		if countOnly {
			pattern += "/count"
		}
		routeAggregate(mux, pattern, gateViewer, mgr, func(m *fleet.DatabaseManager) http.Handler {
			return inboxHandler(m, ledgers, countOnly)
		})
	}
}

// inboxQuery is a validated ?database=&kinds=&limit=.
type inboxQuery struct {
	database string
	kinds    []inbox.Kind
	limit    int
}

func parseInboxQuery(w http.ResponseWriter, r *http.Request) (inboxQuery, bool) {
	q := r.URL.Query()
	db, ok := readDatabaseParam(w, r)
	if !ok {
		return inboxQuery{}, false
	}
	kinds, err := inbox.ParseKinds(q.Get("kinds"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return inboxQuery{}, false
	}
	limit := inbox.DefaultLimit
	if raw, set := q["limit"]; set {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > inbox.MaxLimit {
			jsonError(w, fmt.Sprintf("limit must be an integer from 1 to %d",
				inbox.MaxLimit), http.StatusBadRequest)
			return inboxQuery{}, false
		}
		limit = n
	}
	return inboxQuery{database: oneDatabase(db), kinds: kinds, limit: limit}, true
}

func inboxHandler(mgr *fleet.DatabaseManager, ledgers *earned.Registry,
	countOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, ok := parseInboxQuery(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, q.database) {
			return
		}
		res := listInbox(r.Context(), mgr, ledgers, q, inboxDatabases(mgr, nil))
		if user := UserFromContext(r.Context()); user != nil {
			for i := range res.Items {
				res.Items[i].Actionable = inbox.CanDecide(res.Items[i].Kind, user.Role)
			}
		}
		if countOnly {
			jsonResponse(w, map[string]any{"total": res.Total,
				"counts_by_kind": res.CountsByKind, "lower_bound": res.LowerBound,
				"errors": res.Errors})
			return
		}
		jsonResponse(w, res)
	}
}

// inboxDatabases is the fleet's database names, kept to allowed when it is
// not nil (an MCP principal restricted to some databases).
func inboxDatabases(mgr *fleet.DatabaseManager, allowed []string) []string {
	if mgr == nil {
		return []string{}
	}
	keep := map[string]bool{}
	for _, name := range allowed {
		keep[name] = true
	}
	names := []string{}
	for _, inst := range sortedFleetInstances(mgr) {
		if allowed == nil || keep[inst.Name] {
			names = append(names, inst.Name)
		}
	}
	return names
}

// listInbox lists q over names (or over q.database alone).
func listInbox(ctx context.Context, mgr *fleet.DatabaseManager, ledgers *earned.Registry,
	q inboxQuery, names []string) inbox.Result {
	if q.database != "" {
		names = []string{q.database}
	}
	var in inbox.Instances
	if mgr != nil {
		in = mgr
	}
	svc := &inbox.Service{Sources: inbox.FleetSources(in, ledgers)}
	return svc.List(ctx, inbox.Request{Databases: names, Kinds: q.kinds, Limit: q.limit})
}

// InboxMCP serves get_inbox from the same read as GET /api/v1/inbox. An
// agent never decides from it: every item is actionable=false.
func InboxMCP(mgr *fleet.DatabaseManager, ledgers *earned.Registry) mcp.InboxBackend {
	return inboxMCP{mgr: mgr, ledgers: ledgers}
}

type inboxMCP struct {
	mgr     *fleet.DatabaseManager
	ledgers *earned.Registry
}

func (b inboxMCP) Inbox(ctx context.Context, req mcp.InboxRequest) (any, error) {
	kinds, err := inbox.KindsOf(req.Kinds)
	if err != nil {
		return nil, err
	}
	names := inboxDatabases(b.mgr, req.Databases)
	if req.Database != "" && !contains(names, req.Database) {
		return nil, mcp.ErrInboxDatabaseNotFound
	}
	limit := req.Limit
	if limit == 0 {
		limit = inbox.DefaultLimit
	}
	return listInbox(ctx, b.mgr, b.ledgers, inboxQuery{database: req.Database,
		kinds: kinds, limit: limit}, names), nil
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
