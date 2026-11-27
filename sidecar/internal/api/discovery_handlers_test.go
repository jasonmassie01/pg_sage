package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// No concurrent access tests here: concurrent claims and transitions are
// covered against PostgreSQL in internal/store and internal/fleet/discovery.

func TestDiscoveredRoutes_AbsentWithoutDiscoveryStore(t *testing.T) {
	mux := http.NewServeMux()
	registerDatabaseRoutes(mux, &DatabaseDeps{})
	req := withUser(httptest.NewRequest("GET", "/api/v1/databases/discovered", nil),
		testAdminUser())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when discovery is not wired", w.Code)
	}
}

func TestDiscoveredHandlers_InvalidInput(t *testing.T) {
	deps := &DatabaseDeps{Discovery: store.NewDiscoveryStore(nil)}
	cases := []struct {
		name, id, body string
		h              http.HandlerFunc
		want           int
	}{
		{"adopt bad id", "x", "", adoptDiscoveredHandler(deps), http.StatusBadRequest},
		{"ignore bad id", "-1", "", ignoreDiscoveredHandler(deps), http.StatusBadRequest},
		{"extend bad body", "1", "{", extendDiscoveredHandler(deps), http.StatusBadRequest},
		{"extend zero hours", "1", `{"hours":0}`, extendDiscoveredHandler(deps),
			http.StatusBadRequest},
		{"extend too many hours", "1", `{"hours":721}`, extendDiscoveredHandler(deps),
			http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
			req.SetPathValue("id", tc.id)
			w := httptest.NewRecorder()
			tc.h(w, withUser(req, testAdminUser()))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// seedDiscovered records two candidates of source c and enrols the first.
func seedDiscovered(t *testing.T) (*DatabaseDeps, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.discovered_databases;
		DELETE FROM sage.discovery_sources; DELETE FROM sage.databases`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	ds, disc := store.NewDatabaseStore(pool, p2Key), store.NewDiscoveryStore(pool)
	deps := &DatabaseDeps{Store: ds, Discovery: disc}
	memberID, err := ds.Create(ctx, store.DatabaseInput{Name: "main", Host: "h", Port: 5432,
		DatabaseName: "postgres", Username: "u", Password: "p", SSLMode: "require",
		MaxConnections: 2, TrustLevel: "observation", ExecutionMode: "approval"}, 1)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	now := time.Now()
	_, _ = disc.Upsert(ctx, "c", "team", []store.DiscoveredCandidate{
		{ExternalID: "h:5432/a", Host: "h", Port: 5432, DBName: "a"},
		{ExternalID: "h:5432/b", Host: "h", Port: 5432, DBName: "b"}}, now)
	_ = disc.RecordSourceRun(ctx, "c", 2, nil, now)
	rows, _ := disc.Claim(ctx, "c", 1, 5, now)
	dbID, err := ds.CreateDiscovered(ctx, memberID, store.DiscoveredMember{Name: "c-a-1",
		DatabaseName: "a", Source: "c", Owner: "team", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateDiscovered: %v", err)
	}
	if err := disc.MarkEnrolled(ctx, rows[0].ID, dbID); err != nil {
		t.Fatalf("MarkEnrolled: %v", err)
	}
	return deps, pool, ctx
}

func TestDiscoveredHandlers_RealDB(t *testing.T) {
	deps, pool, ctx := seedDiscovered(t)
	listed := listDiscovered(t, deps)
	if len(listed.Discovered) != 2 || len(listed.Sources) != 1 ||
		listed.Sources[0]["source"] != "c" {
		t.Fatalf("list = %+v", listed)
	}
	enrolled, candidate := rowWith(listed, "a"), rowWith(listed, "b")
	if enrolled["status"] != "enrolled" || enrolled["expires_at"] == nil ||
		candidate["status"] != "candidate" || candidate["owner"] != "team" {
		t.Fatalf("rows = %+v / %+v", enrolled, candidate)
	}
	id := func(r map[string]any) string { return strconv.Itoa(int(r["id"].(float64))) }
	code := postDiscovered(t, extendDiscoveredHandler(deps), id(enrolled), `{"hours":24}`)
	if code != 200 {
		t.Fatalf("extend = %d", code)
	}
	if code := postDiscovered(t, adoptDiscoveredHandler(deps), id(candidate), ""); code != 400 {
		t.Fatalf("adopting a candidate = %d, want 400", code)
	}
	if code := postDiscovered(t, adoptDiscoveredHandler(deps), id(enrolled), ""); code != 200 {
		t.Fatalf("adopt = %d", code)
	}
	if code := postDiscovered(t, ignoreDiscoveredHandler(deps), id(candidate), ""); code != 200 {
		t.Fatalf("ignore = %d", code)
	}
	if code := postDiscovered(t, ignoreDiscoveredHandler(deps), "999999", ""); code != 404 {
		t.Fatalf("ignoring a missing row = %d, want 404", code)
	}
	after := listDiscovered(t, deps)
	if rowWith(after, "a")["status"] != "adopted" || rowWith(after, "b")["status"] != "ignored" {
		t.Fatalf("after actions = %+v", after.Discovered)
	}
	var audits int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE key IN ('fleet.discovery.adopted', 'fleet.discovery.ignored',
		              'fleet.discovery.extend') AND changed_by = 1`).Scan(&audits)
	if audits != 3 {
		t.Fatalf("audited user actions = %d, want 3", audits)
	}
}

type discoveredList struct {
	Discovered []map[string]any `json:"discovered"`
	Sources    []map[string]any `json:"sources"`
}

func listDiscovered(t *testing.T, deps *DatabaseDeps) discoveredList {
	t.Helper()
	w := httptest.NewRecorder()
	listDiscoveredHandler(deps)(w, withUser(httptest.NewRequest("GET",
		"/api/v1/databases/discovered", nil), testAdminUser()))
	if w.Code != 200 {
		t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
	}
	var out discoveredList
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func rowWith(l discoveredList, dbname string) map[string]any {
	for _, r := range l.Discovered {
		if r["dbname"] == dbname {
			return r
		}
	}
	return map[string]any{}
}

func postDiscovered(t *testing.T, h http.HandlerFunc, id, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h(w, withUser(req, testAdminUser()))
	return w.Code
}

// CHECK-02 through the API: raising an unadopted discovered member's trust
// is refused with 409 and the row keeps observation.
func TestConfigDBPut_DiscoveredMemberTrustIsConflict(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	const databaseID = 9911
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	resetWave2DatabaseConfig(t, ctx, pool, databaseID)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(id, name, host, port, database_name, username, password_enc, sslmode,
		 trust_level, execution_mode, discovered_source, owner, expires_at)
		VALUES ($1, 'disc-api-guard', 'localhost', 5432, 'postgres', 'postgres', '\x00',
		 'disable', 'observation', 'approval', 'cluster', 'team', now() + interval '1h')`,
		databaseID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "disc-api-guard",
		DatabaseID: databaseID, Config: config.DatabaseConfig{Name: "disc-api-guard",
			TrustLevel: "observation", TrustLevelExplicit: true},
		Status: &fleet.InstanceStatus{TrustLevel: "observation"}})
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/config/databases/{id}",
		configDBPutHandler(store.NewConfigStore(pool), config.DefaultConfig(), pool, mgr))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/databases/9911",
		strings.NewReader(`{"expected_generation":1,"trust.level":"advisory"}`))
	mux.ServeHTTP(w, withUser(req, user))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "adopt") {
		t.Fatalf("status = %d body=%s, want 409 naming adoption", w.Code, w.Body.String())
	}
	var trust string
	_ = pool.QueryRow(ctx, `SELECT trust_level FROM sage.databases WHERE id = $1`,
		databaseID).Scan(&trust)
	if trust != "observation" {
		t.Fatalf("trust = %q after a refused raise", trust)
	}
}
