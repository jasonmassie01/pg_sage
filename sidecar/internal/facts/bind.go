package facts

import (
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Route is where work a fact binds goes instead.
type Route string

// Routes.
const (
	// RouteSourceFix: the change belongs in the application's migrations;
	// pg_sage attaches the migration it recommends (a source-fix packet).
	RouteSourceFix Route = "source_fix"
	// RouteAlert: a protected slot is never dropped or advanced; pg_sage
	// alerts instead.
	RouteAlert Route = "alert"
	// RouteKeep: an archive keeps its data and indexes.
	RouteKeep Route = "keep"
	// RouteExcluded: test fixtures are not workload; pg_sage leaves them
	// alone and offers one operator-approved cleanup batch.
	RouteExcluded Route = "excluded"
	// RouteWait: the table's window decides when work may run.
	RouteWait Route = "wait_for_window"
)

// Request is what a binding is judged on: the action, its SQL, its target
// objects and whether a person approved it.
type Request struct {
	ActionType       string
	SQL              string
	Targets          []string
	OperatorApproved bool
	Now              time.Time
}

// Binding is one confirmed fact that binds a request.
type Binding struct {
	Fact   Fact
	Object string
	Route  Route
}

// ddlActions change an object's definition; exemptActions undo pg_sage's
// own change and are never bound.
var (
	ddlActions = map[string]bool{"create_index_concurrently": true,
		"drop_unused_index": true, "alter_table": true, "set_table_autovacuum": true,
		"create_statistics": true, "replace_index": true}
	exemptActions = map[string]bool{"revert_created_index": true}
	ddlVerbs      = map[string]bool{"CREATE INDEX": true, "CREATE STATISTICS": true,
		"CREATE TABLE": true, "CREATE TRIGGER": true, "DROP INDEX": true,
		"DROP STATISTICS": true, "DROP TABLE": true, "DROP SCHEMA": true,
		"ALTER INDEX": true, "ALTER TABLE": true, "ALTER SCHEMA": true,
		"COMMENT ON": true}
	keepActions = map[string]bool{"retention_delete": true, "drop_unused_index": true,
		"plan_bloat_remediation": true, "replace_index": true}
	keepVerbs = map[string]bool{"DELETE": true, "TRUNCATE": true, "DROP INDEX": true,
		"DROP TABLE": true, "CLUSTER": true}
)

// requestClass is what a request does, by its typed action and its SQL.
type requestClass struct {
	exempt, ddl, rewritesData bool
}

func classify(req Request) requestClass {
	if exemptActions[req.ActionType] {
		return requestClass{exempt: true}
	}
	toks := scanSQL(req.SQL)
	v := verb(toks)
	return requestClass{
		ddl:          ddlActions[req.ActionType] || ddlVerbs[v],
		rewritesData: keepActions[req.ActionType] || keepVerbs[v] || vacuumFull(v, toks),
	}
}

func vacuumFull(v string, toks []token) bool {
	if v != "VACUUM" {
		return false
	}
	for _, t := range toks {
		if t.kind == tokWord && strings.EqualFold(t.text, "FULL") {
			return true
		}
	}
	return false
}

// Bind returns the confirmed, unexpired facts that bind a request touching
// refs, at most one binding per fact, in fact order.
func Bind(facts []Fact, refs []ObjectRef, req Request) []Binding {
	if len(facts) == 0 || len(refs) == 0 {
		return nil
	}
	class := classify(req)
	if class.exempt {
		return nil
	}
	var out []Binding
	for _, f := range facts {
		if !f.binds(req.Now) {
			continue
		}
		route, applies := routeFor(f, class, req)
		if !applies {
			continue
		}
		if obj, ok := matchRefs(f, refs); ok {
			out = append(out, Binding{Fact: f, Object: obj, Route: route})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Fact.ID < out[j].Fact.ID })
	return out
}

// routeFor reports whether a fact of f's type applies to a request of this
// class at all, and the route it takes.
func routeFor(f Fact, class requestClass, req Request) (Route, bool) {
	switch f.Type {
	case TypeAppMigrations:
		return RouteSourceFix, class.ddl
	case TypeSlotConsumer:
		return RouteAlert, true
	case TypeAppendOnly:
		return RouteKeep, class.rewritesData
	case TypeTestFixture:
		return RouteExcluded, !req.OperatorApproved
	case TypeTableWindow:
		return RouteWait, !req.OperatorApproved && windowHolds(f, req.Now)
	}
	return "", false
}

// windowHolds reports whether a table window holds work at now: outside a
// maintenance window, or inside a batch window. An unreadable window
// holds (a fact only narrows).
func windowHolds(f Fact, now time.Time) bool {
	w, err := policy.ParseWindow(f.Value["window"])
	if err != nil {
		return true
	}
	inside := w.Contains(now)
	if f.Value["kind"] == "batch" {
		return inside
	}
	return !inside
}

// matchRefs returns the first ref the fact's subject covers.
func matchRefs(f Fact, refs []ObjectRef) (string, bool) {
	p, err := ParsePattern(f.Kind, f.Subject)
	if err != nil {
		return "", false
	}
	for _, ref := range refs {
		if p.Matches(ref) {
			return ref.String(), true
		}
	}
	return "", false
}
