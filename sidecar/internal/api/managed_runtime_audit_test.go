package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestAuditManagedConnectionUsesAuthenticatedPool(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://audit_user:synthetic%40secret@127.0.0.1:5455/audit?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "managed", DatabaseID: 71, Pool: pool,
		Config: config.DatabaseConfig{Name: "managed", Host: "127.0.0.1",
			Port: 5455, User: "audit_user", Database: "audit", SSLMode: "disable"},
	})
	dsn, password, found := fleetManagedDBConnection(&DatabaseDeps{Fleet: mgr}, 71, "")
	parsed, err := url.Parse(dsn)
	if err != nil || !found {
		t.Fatalf("runtime connection unavailable: found=%v parse=%v", found, err)
	}
	parsedPassword, _ := parsed.User.Password()
	if password != "synthetic@secret" || parsedPassword != password {
		t.Fatal("runtime connection lost the authenticated pool credential")
	}
	if parsed.Host != "127.0.0.1:5455" || parsed.Path != "/audit" {
		t.Fatal("runtime connection changed its target")
	}
}

func TestAuditSnapshotDefaultUsesCollectedSystemCategory(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, err := pool.Exec(ctx, "INSERT INTO sage.snapshots (category,data) VALUES ($1,$2)",
		"system", `{"connections_active":7}`)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/snapshots/latest?database=testdb", nil)
	snapshotLatestHandler(phase2MgrWithPool(pool)).ServeHTTP(w, r)
	var body struct {
		Database string `json:"database"`
		Snapshot struct {
			Connections int `json:"connections_active"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body.Database != "testdb" || body.Snapshot.Connections != 7 {
		t.Fatalf("default snapshot omitted collected system data: status=%d value=%+v", w.Code, body)
	}
}

func TestAuditSnapshotQueryFailureIsNotEmptySuccess(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/api/v1/snapshots/latest?database=testdb", nil)
	w := httptest.NewRecorder()
	snapshotLatestHandler(phase2MgrWithPool(pool)).ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 500 {
		t.Fatalf("database query failure masqueraded as empty success: status=%d", w.Code)
	}
}

func TestAuditSnapshotMissingCategoryIsEmpty(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	r := httptest.NewRequest("GET",
		"/api/v1/snapshots/latest?database=testdb&metric=absent_audit_category", nil)
	w := httptest.NewRecorder()
	snapshotLatestHandler(phase2MgrWithPool(pool)).ServeHTTP(w, r)
	var body struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || string(body.Snapshot) != "null" {
		t.Fatalf("missing category contract changed: status=%d", w.Code)
	}
}
