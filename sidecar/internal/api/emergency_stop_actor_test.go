package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func sessionAs(user *auth.User) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, user))
		})
	}
}

// stopTestDB is a throwaway database: a persisted emergency stop must not
// leak into other packages that share SAGE_TEST_DATABASE_URL.
func stopTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, err := testdb.DesignatedDSN()
	if err != nil {
		t.Skipf("%v; actor persistence needs PostgreSQL", err)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	ident := pgx.Identifier{fmt.Sprintf("sage_estop_api_%d", time.Now().UnixNano())}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = ident[0]
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+ident.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func stopRouter(
	t *testing.T, pool *pgxpool.Pool, user *auth.User,
) (http.Handler, *fleet.DatabaseManager) {
	t.Helper()
	cfg := &config.Config{Mode: "fleet", Trust: config.TrustConfig{Level: "autonomous"}}
	mgr := fleet.NewManager(cfg)
	inst := &fleet.DatabaseInstance{
		Name:   "orders",
		Config: config.DatabaseConfig{Name: "orders"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()},
	}
	if pool != nil {
		inst.Pool = pool
		inst.Executor = executor.New(pool, cfg, nil, time.Now(),
			func(string, string, ...any) {})
	}
	mgr.RegisterInstance(inst)
	return NewRouter(mgr, cfg, nil, sessionAs(user)), mgr
}

func persistedStopActor(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	var value, by string
	err := pool.QueryRow(context.Background(),
		`SELECT value, updated_by FROM sage.config WHERE key = 'emergency_stop'`,
	).Scan(&value, &by)
	if err != nil {
		t.Fatalf("read emergency_stop: %v", err)
	}
	return value, by
}

type databaseStopView struct {
	Name               string     `json:"name"`
	EmergencyStopped   bool       `json:"emergency_stopped"`
	EmergencyStoppedBy string     `json:"emergency_stopped_by"`
	EmergencyStoppedAt *time.Time `json:"emergency_stopped_at"`
}

func fleetStopView(t *testing.T, r http.Handler) databaseStopView {
	t.Helper()
	w := get(t, r, "/api/v1/databases")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /databases: %d", w.Code)
	}
	var body struct {
		Databases []databaseStopView `json:"databases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Databases) != 1 {
		t.Fatalf("databases = %+v, want one", body.Databases)
	}
	return body.Databases[0]
}

func TestEmergencyStopRecordsSessionActor(t *testing.T) {
	pool := stopTestDB(t)
	r, _ := stopRouter(t, pool, testOperatorUser())

	w := post(t, r, "/api/v1/emergency-stop?database=orders", "")

	if w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	value, by := persistedStopActor(t, pool)
	if value != "true" || by != "operator@test.com" {
		t.Fatalf("sage.config = %s by %q, want true by operator@test.com", value, by)
	}
	view := fleetStopView(t, r)
	if !view.EmergencyStopped || view.EmergencyStoppedBy != "operator@test.com" ||
		view.EmergencyStoppedAt == nil {
		t.Fatalf("fleet view = %+v, want stopped by operator@test.com", view)
	}
}

func TestResumeRecordsSessionActor(t *testing.T) {
	pool := stopTestDB(t)
	r, mgr := stopRouter(t, pool, testAdminUser())
	if _, err := mgr.EmergencyStopStrict("orders", "operator@test.com"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	w := post(t, r, "/api/v1/resume?database=orders", "")

	if w.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	value, by := persistedStopActor(t, pool)
	if value != "false" || by != "admin@test.com" {
		t.Fatalf("sage.config = %s by %q, want false by admin@test.com", value, by)
	}
	view := fleetStopView(t, r)
	if view.EmergencyStopped || view.EmergencyStoppedBy != "" {
		t.Fatalf("fleet view = %+v, want running", view)
	}
}

func TestEmergencyStopViewerForbidden(t *testing.T) {
	r, mgr := stopRouter(t, nil, testViewerUser())

	for _, path := range []string{
		"/api/v1/emergency-stop?database=orders",
		"/api/v1/emergency-stop",
		"/api/v1/resume?database=orders",
	} {
		w := post(t, r, path, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("viewer POST %s = %d, want 403", path, w.Code)
		}
	}
	if mgr.InstanceStopped(mgr.GetInstance("orders")) {
		t.Fatal("a viewer request must not stop the database")
	}
}
