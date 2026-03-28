//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres" +
		"?sslmode=disable"
}

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
	testKey      = crypto.DeriveKey("integration-test-key")
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	testPoolOnce.Do(func() {
		poolCfg, err := pgxpool.ParseConfig(testDSN())
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 1
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		// Ensure schema and databases table exist.
		if err := schema.Bootstrap(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			return
		}
		schema.ReleaseAdvisoryLock(ctx, testPool)
		if err := schema.EnsureDatabasesTable(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("ensure databases: %w", err)
			return
		}
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	// Clean table before each test.
	_, _ = testPool.Exec(ctx, "DELETE FROM sage.databases")
	return testPool, ctx
}

func TestCreateDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	id, err := store.Create(ctx, validInput(), 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id < 1 {
		t.Errorf("expected positive ID, got %d", id)
	}
}

func TestCreateDatabaseDuplicate(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	_, err := store.Create(ctx, validInput(), 1)
	if err != nil {
		t.Fatalf("Create (first): %v", err)
	}

	_, err = store.Create(ctx, validInput(), 1)
	if err == nil {
		t.Error("expected duplicate name error, got nil")
	}
}

func TestListDatabases(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	for i := range 3 {
		input := validInput()
		input.Name = fmt.Sprintf("db-%d", i)
		if _, err := store.Create(ctx, input, 1); err != nil {
			t.Fatalf("Create db-%d: %v", i, err)
		}
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("List returned %d records, want 3", len(list))
	}
}

func TestGetDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	id, err := store.Create(ctx, validInput(), 42)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Name != "prod-db" {
		t.Errorf("Name = %q, want %q", rec.Name, "prod-db")
	}
	if rec.CreatedBy != 42 {
		t.Errorf("CreatedBy = %d, want 42", rec.CreatedBy)
	}
	if rec.Tags["env"] != "prod" {
		t.Errorf("Tags[env] = %q, want %q", rec.Tags["env"], "prod")
	}
}

func TestUpdateDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	id, err := store.Create(ctx, validInput(), 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated := validInput()
	updated.Name = "staging-db"
	updated.Host = "staging.example.com"
	updated.Password = "new-password"

	if err := store.Update(ctx, id, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rec, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if rec.Name != "staging-db" {
		t.Errorf("Name = %q, want %q", rec.Name, "staging-db")
	}
	if rec.Host != "staging.example.com" {
		t.Errorf("Host = %q, want %q", rec.Host, "staging.example.com")
	}
}

func TestUpdateDatabasePasswordOptional(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	id, err := store.Create(ctx, validInput(), 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Get connection string before update.
	before, err := store.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("GetConnectionString before: %v", err)
	}

	// Update without password.
	updated := validInput()
	updated.Password = ""
	updated.Name = "renamed-db"
	if err := store.Update(ctx, id, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Password should be unchanged.
	after, err := store.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("GetConnectionString after: %v", err)
	}

	// Extract password portion from both connection strings.
	if !strings.Contains(before, "s3cret") {
		t.Errorf("before connstr missing password: %s", before)
	}
	if !strings.Contains(after, "s3cret") {
		t.Errorf("after connstr missing password: %s", after)
	}
}

func TestDeleteDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	id, err := store.Create(ctx, validInput(), 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	count, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("Count = %d after delete, want 0", count)
	}
}

func TestGetConnectionString(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	id, err := store.Create(ctx, validInput(), 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	connStr, err := store.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("GetConnectionString: %v", err)
	}

	want := "postgres://admin:s3cret@db.example.com:5432/myapp" +
		"?sslmode=require"
	if connStr != want {
		t.Errorf("connection string:\n got  %s\n want %s", connStr, want)
	}
}

func TestMaxDatabasesLimit(t *testing.T) {
	pool, ctx := requireDB(t)
	store := NewDatabaseStore(pool, testKey)

	for i := range 50 {
		input := validInput()
		input.Name = fmt.Sprintf("db-%03d", i)
		if _, err := store.Create(ctx, input, 1); err != nil {
			t.Fatalf("Create db-%03d: %v", i, err)
		}
	}

	// 51st should fail.
	input := validInput()
	input.Name = "db-050"
	_, err := store.Create(ctx, input, 1)
	if err == nil {
		t.Error("expected max databases error, got nil")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error should mention maximum: %v", err)
	}
}
