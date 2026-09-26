package api

import (
	"context"
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/auth"
)

// Test-only router helpers (G8-D05): production wires
// registerAgentDBRoutesWithAuthority from router.go.

func registerAgentDBRoutes(
	mux *http.ServeMux,
	st *agentdb.Store,
	generators ...agentdb.BlueprintGenerator,
) {
	var blueprintGenerator agentdb.BlueprintGenerator
	if len(generators) > 0 {
		blueprintGenerator = generators[0]
	}
	registerAgentDBRoutesWithAuthority(mux, st, blueprintGenerator, nil)
}

// agentDBSubrouter serves the AgentDB subrouter for handler tests. Routes
// are session-authenticated in production, so requests without a user get a
// fixed operator identity for audit-actor fields.
func agentDBSubrouter(st *agentdb.Store) http.HandlerFunc {
	return withTestOperator(
		agentDBSubrouterWithRegistry(st, agentdb.DefaultRunnerRegistry(), nil),
	).ServeHTTP
}

// withTestOperator gives handler tests an authenticated operator session.
func withTestOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserFromContext(r.Context()) == nil {
			user := &auth.User{ID: 900, Email: "operator@test.invalid", Role: "operator"}
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		}
		next.ServeHTTP(w, r)
	})
}
