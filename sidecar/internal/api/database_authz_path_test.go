package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-08 review: the path resolvers ({db} on the per-database SRE mux,
// {database} on agent classification) read the same path value their
// handlers do, through the production fleet router. On the granted
// database the request reaches the handler (whatever it answers, it is not
// database_forbidden); on another database it is refused before it.
// No DB or concurrency test here: dbAuthzRouter uses an in-memory source.

func TestDatabaseAuthz_PathResolversThroughTheRouter(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzRouter(t, true, src, dbOperator(7))
	paths := []string{
		"/api/v1/databases/%s/investigations",
		"/api/v1/databases/%s/investigations/4/stop",
		"/api/v1/databases/%s/investigations/4/proposals",
		"/api/v1/databases/%s/runbooks",
		"/api/v1/agent-classifications/%s/scan",
		"/api/v1/agent-classifications/%s/4/confirm",
	}
	for _, p := range paths {
		code, body := dbAuthzDo(h, "POST", fmt.Sprintf(p, "B"), `{}`, nil)
		requireForbidden(t, "router: "+fmt.Sprintf(p, "B"), code, body)
		code, body = dbAuthzDo(h, "POST", fmt.Sprintf(p, "A"), `{}`, nil)
		if body["code"] == "database_forbidden" || code == http.StatusServiceUnavailable &&
			body["code"] == "grant_store_unavailable" {
			t.Errorf("router: %s: %d %v, want the handler's own answer",
				fmt.Sprintf(p, "A"), code, body)
		}
	}
}

func TestDatabaseAuthz_AdminUnaffectedByAFailingStore(t *testing.T) {
	src := &dbAuthzSource{err: fmt.Errorf("connection refused")}
	admin := &auth.User{ID: 1, Role: auth.RoleAdmin}
	code, body := dbAuthzDo(dbAuthzMux(src, nil), "POST",
		"/api/v1/actions/3/approve?database=B", "{}", admin)
	requireReached(t, "admin with a failing grant store", code, body)
}
