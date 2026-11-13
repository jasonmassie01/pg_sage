package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// market-11: analyzer.disabled_rules is a per-database override.

func putDBConfig(h http.HandlerFunc, id, body string, user *auth.User) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/config/databases/{id}", h)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/databases/"+id,
		strings.NewReader(body))
	if user != nil {
		req = withUser(req, user)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDisabledRulesOverride_ProtectedAndUnknownAre400(t *testing.T) {
	h := configDBPutHandler(nil, config.DefaultConfig(), nil, nil)
	for value, want := range map[string]string{
		"wraparound_freeze":             "cannot be disabled",
		"unused_indexes,xid_wraparound": "cannot be disabled",
		"sage_self_cost":                "cannot be disabled",
		"no_such_rule":                  "unknown rule or category",
	} {
		rec := putDBConfig(h, "7",
			`{"expected_generation":1,"analyzer.disabled_rules":"`+value+`"}`, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: status %d body %s; want 400 naming %q", value, rec.Code,
				rec.Body.String(), want)
		}
	}
}

func TestDisabledRulesOverride_IsAcceptedAsPerDatabaseKey(t *testing.T) {
	if _, ok := store.AllowedConfigKeysSnapshot()["analyzer.disabled_rules"]; !ok {
		t.Fatal("analyzer.disabled_rules is not a settable key")
	}
	if err := store.ValidateConfigOverride("analyzer.disabled_rules", ""); err != nil {
		t.Fatalf("an empty list must be valid (nothing disabled): %v", err)
	}
}

func TestDisabledRulesOverride_AppliesToTheDatabaseAndAudits(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	const databaseID = 9911
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Email: "dba@example.test",
		Role: auth.RoleAdmin}
	seedRulesDatabase(t, pool, databaseID)
	cs := store.NewConfigStore(pool)
	base := config.DefaultConfig()
	mgr := fleet.NewManager(base)
	an := analyzer.New(nil, base, nil, nil, nil, nil, nil,
		func(string, string, ...any) {})
	inst := &fleet.DatabaseInstance{Name: "market11-rules", DatabaseID: databaseID,
		Config: config.DatabaseConfig{Name: "market11-rules"}, Analyzer: an,
		Status: &fleet.InstanceStatus{}}
	mgr.RegisterInstance(inst)
	rec := putDBConfig(configDBPutHandler(cs, base, pool, mgr), "9911",
		`{"expected_generation":1,"analyzer.disabled_rules":"unused_indexes"}`, user)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "analyzer.disabled_rules") {
		t.Fatalf("put = %d %s, want 200 applying analyzer.disabled_rules", rec.Code,
			rec.Body.String())
	}
	names, ok := an.DisabledRulesOverride()
	if !ok || len(names) != 1 || names[0] != "unused_indexes" {
		t.Fatalf("analyzer override = %v, %v", names, ok)
	}
	var changedBy int
	if err := pool.QueryRow(ctx, `SELECT changed_by FROM sage.config_audit
		WHERE database_id = $1 AND key = 'analyzer.disabled_rules'`, databaseID,
	).Scan(&changedBy); err != nil || changedBy != user.ID {
		t.Fatalf("audit changed_by = %d, %v; want user %d", changedBy, err, user.ID)
	}

	delMux := http.NewServeMux()
	delMux.HandleFunc("DELETE /api/v1/config/databases/{id}/{key}",
		configDBDeleteHandler(cs, base, mgr))
	del := httptest.NewRecorder()
	delMux.ServeHTTP(del, withUser(httptest.NewRequest(http.MethodDelete,
		"/api/v1/config/databases/9911/analyzer.disabled_rules?expected_generation=2",
		nil), user))
	if del.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", del.Code, del.Body.String())
	}
	if _, ok := an.DisabledRulesOverride(); ok {
		t.Fatal("the override survived its delete: the database must inherit again")
	}
}

func seedRulesDatabase(t *testing.T, pool *pgxpool.Pool, databaseID int) {
	t.Helper()
	ctx := context.Background()
	resetWave2DatabaseConfig(t, ctx, pool, databaseID)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(id, name, host, port, database_name, username, password_enc,
		 sslmode, trust_level, execution_mode)
		VALUES ($1, 'market11-rules', 'localhost', 5432, 'postgres',
		 'postgres', '\x00', 'disable', 'observation', 'approval')`, databaseID); err != nil {
		t.Fatalf("seed database: %v", err)
	}
}
