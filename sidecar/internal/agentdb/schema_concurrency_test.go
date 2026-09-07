package agentdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func freshEnsurePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, agentDBTestDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("agentdb_ensure_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+quoted); err != nil {
			t.Error(err)
		}
	})
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database, cfg.MaxConns = name, 12
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertEnsureReady(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM sage.agent_db_size_profiles").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(defaultSizeProfiles()) {
		t.Fatalf("default profiles = %d, want %d", count, len(defaultSizeProfiles()))
	}
	if rows, err := NewStore(pool).ListRequests(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("fresh request listing = %v, %v", rows, err)
	}
}

func TestEnsureConcurrentColdStores(t *testing.T) {
	pool := freshEnsurePool(t)
	start := make(chan struct{})
	results := make(chan error, 8)
	var ready sync.WaitGroup
	ready.Add(cap(results))
	for range cap(results) {
		go func() {
			store := NewStore(pool)
			ready.Done()
			<-start
			results <- store.Ensure(t.Context())
		}()
	}
	ready.Wait()
	close(start)
	for range cap(results) {
		if err := <-results; err != nil {
			t.Errorf("concurrent schema initialization failed: %v", err)
		}
	}
	assertEnsureReady(t, pool)
}

func TestEnsureDatabaseLockCancellationAndRecovery(t *testing.T) {
	pool := freshEnsurePool(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Hold the database-level lock from an independent connection, as another process would.
	if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock($1)",
		int64(0x5047534741474442)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	err = NewStore(pool).Ensure(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock waiter lost deadline error: %v", err)
	}
	var absent bool
	if err := pool.QueryRow(t.Context(),
		"SELECT to_regnamespace('sage') IS NULL").Scan(&absent); err != nil || !absent {
		t.Fatalf("canceled lock waiter modified schema: absent=%v err=%v", absent, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(pool).Ensure(t.Context()); err != nil {
		t.Fatalf("initialization did not recover after cancellation: %v", err)
	}
	assertEnsureReady(t, pool)
}

func TestEnsureSeedFailureRollsBackSchemaAndReleasesLock(t *testing.T) {
	pool := freshEnsurePool(t)
	for _, sql := range []string{"CREATE SCHEMA sage",
		"CREATE TABLE sage.agent_db_size_profiles (profile_id integer PRIMARY KEY)"} {
		if _, err := pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	err := NewStore(pool).Ensure(t.Context())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42703" {
		t.Fatalf("seed failure lost missing-column SQLSTATE: %v", err)
	}
	var absent bool
	if err := pool.QueryRow(t.Context(),
		"SELECT to_regclass('sage.agent_identities') IS NULL").Scan(&absent); err != nil || !absent {
		t.Fatalf("failed initialization left partially committed DDL: absent=%v err=%v", absent, err)
	}
	if _, err := pool.Exec(t.Context(), "DROP TABLE sage.agent_db_size_profiles"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := NewStore(pool).Ensure(ctx); err != nil {
		t.Fatalf("initialization did not recover after seed failure: %v", err)
	}
	assertEnsureReady(t, pool)
}
