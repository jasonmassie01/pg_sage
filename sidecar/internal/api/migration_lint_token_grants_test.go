package api

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// apiauth-08c x migration-11b: with api.database_grants on, a person's
// read-scope token lints only the databases on its own list that its
// owner may read now; another database is 404 like an unknown one, and a
// grant-store failure refuses the request (503) without reading a catalog.
// Agent tokens keep their own lists, and with grants off nothing changes.
// No concurrent access test here: the grant check is per request and holds
// no state (the store's own concurrency is covered by database_authz_test).

// lintGrantsMux serves token t1 (person, every database), t4 (person,
// alpha only) and ta (agent, every database), all owned by user 2, behind
// the grant source src (nil: grants off). reads counts catalog reads.
func lintGrantsMux(src DatabaseGrantSource, reads *atomic.Int32) http.Handler {
	agent := readGrant("ta")
	agent.Kind = mcptoken.KindAgent
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1"),
		lintSecret(4): readGrant("t4", "alpha"), lintSecret(9): agent})
	catalog := &lintCatalog{rows: 10}
	d := lintDeps(catalog, 1000)
	d.querier = func(string) (catalogread.Querier, error) {
		reads.Add(1)
		return catalog, nil
	}
	d.resolve = func(db string) (string, error) { return db, nil }
	mux := lintTokenMux(d, tokens)
	if src == nil {
		return mux
	}
	return withDatabaseAuthz(&databaseAuthz{grants: src}, mux)
}

func lintOn(db string) string {
	return `{"sql":"CREATE INDEX x ON app.t (c)","database":"` + db + `"}`
}

func TestMigrationLintToken_OwnerGrantsLimitAPersonsToken(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(2, map[string]string{"alpha": auth.RoleViewer})
	var reads atomic.Int32
	h := lintGrantsMux(src, &reads)
	if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), lintOn("alpha")); rec.Code !=
		http.StatusOK {
		t.Fatalf("token on a granted database: %d %s, want 200", rec.Code, rec.Body)
	}
	before := reads.Load()
	hidden := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), lintOn("beta"))
	unlisted := postLintAuth(t, h, nil, "Bearer "+lintSecret(4), lintOn("beta"))
	if hidden.Code != http.StatusNotFound || hidden.Body.String() != unlisted.Body.String() {
		t.Fatalf("every-database token on an ungranted database: %d %s, want the 404 of a "+
			"database off the token's list (%d %s)", hidden.Code, hidden.Body,
			unlisted.Code, unlisted.Body)
	}
	if n := reads.Load() - before; n != 0 {
		t.Fatalf("%d catalog reads for refused databases, want 0", n)
	}
	if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(9), lintOn("beta")); rec.Code !=
		http.StatusOK {
		t.Fatalf("agent token keeps its own list: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestMigrationLintToken_GrantStoreFailureRefuses(t *testing.T) {
	var reads atomic.Int32
	h := lintGrantsMux(&dbAuthzSource{err: errors.New("connection refused")}, &reads)
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), lintOn("alpha"))
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "grant_store_unavailable") {
		t.Fatalf("grant store failure: %d %s, want 503 grant_store_unavailable", rec.Code,
			rec.Body)
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("%d catalog reads after a failed grant check, want 0", n)
	}
}

func TestMigrationLintToken_GrantsOffKeepTheTokensList(t *testing.T) {
	var reads atomic.Int32
	h := lintGrantsMux(nil, &reads)
	if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), lintOn("beta")); rec.Code !=
		http.StatusOK {
		t.Fatalf("grants off, every-database token: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(4), lintOn("beta")); rec.Code !=
		http.StatusNotFound {
		t.Fatalf("grants off, alpha-only token on beta: %d %s, want 404", rec.Code, rec.Body)
	}
}
