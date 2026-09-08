package optimizer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHypoPGNilPoolFailsClosed(t *testing.T) {
	h := NewHypoPG(nil, 1, noopLog2)
	if h.IsAvailable(t.Context()) {
		t.Fatal("nil pool reported HypoPG available")
	}
	ok, improvement, size, err := h.Validate(t.Context(), Recommendation{}, nil)
	if err == nil || ok || improvement != 0 || size != 0 {
		t.Fatalf("nil pool must fail closed: %t %f %d %v", ok, improvement, size, err)
	}
}

func hypopgSessionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := connectTestDB(t)
	t.Cleanup(admin.Close)
	var available bool
	if err := admin.QueryRow(t.Context(), `SELECT EXISTS
		(SELECT 1 FROM pg_available_extensions WHERE name='hypopg')`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Skip("HypoPG server extension unavailable; session integration not verified")
	}
	for _, sql := range []string{
		"CREATE SCHEMA IF NOT EXISTS hypopg_session_test",
		"CREATE EXTENSION IF NOT EXISTS hypopg WITH SCHEMA hypopg_session_test",
		"ALTER EXTENSION hypopg SET SCHEMA hypopg_session_test",
		"CREATE TABLE IF NOT EXISTS hypopg_session_test.items (id int, category int)",
		"TRUNCATE hypopg_session_test.items",
		"INSERT INTO hypopg_session_test.items SELECT i,i%10000 FROM generate_series(1,20000) i",
		"ANALYZE hypopg_session_test.items",
	} {
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	cfg := admin.Config().Copy()
	cfg.MaxConns = 1 // A second pool checkout must never be needed for session-local size.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "17s"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestHypoPGNamespaceSizeAndTimeoutIsolation(t *testing.T) {
	pool := hypopgSessionPool(t)
	h := NewHypoPG(pool, 1, noopLog2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items (category)"}
	queries := []QueryInfo{{QueryID: 1,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}
	ok, improvement, size, err := h.Validate(ctx, rec, queries)
	if err != nil || !ok || improvement <= 1 || size <= 0 {
		t.Fatalf("namespace/session validation: ok=%t improvement=%f size=%d err=%v",
			ok, improvement, size, err)
	}
	assertHypoPGSessionClean(t, pool)
	var realIndexes int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_indexes
		WHERE schemaname='hypopg_session_test' AND tablename='items'`).Scan(&realIndexes); err != nil {
		t.Fatal(err)
	}
	if realIndexes != 0 {
		t.Fatal("hypothetical validation created a persistent index")
	}
}

func TestHypoPGNormalizedWorkloadParameters(t *testing.T) {
	pool := hypopgSessionPool(t)
	h := NewHypoPG(pool, 1, noopLog2)
	rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items (category)"}
	queries := []QueryInfo{{QueryID: 1,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=$1"}}
	ok, improvement, size, err := h.Validate(t.Context(), rec, queries)
	if err != nil || !ok || improvement <= 1 || size <= 0 {
		t.Fatalf("normalized workload: ok=%t improvement=%f size=%d err=%v",
			ok, improvement, size, err)
	}
	assertHypoPGSessionClean(t, pool)
}

func TestHypoPGFailureAndEmptyQueriesCleanSession(t *testing.T) {
	pool := hypopgSessionPool(t)
	h := NewHypoPG(pool, 1, noopLog2)
	rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items (category)"}
	bad := []QueryInfo{{QueryID: 1, Text: "SELECT missing_column FROM hypopg_session_test.items"}}
	if ok, _, _, err := h.Validate(t.Context(), rec, bad); err == nil || ok {
		t.Fatal("invalid EXPLAIN must return an actionable error, not an unvalidated success")
	}
	assertHypoPGSessionClean(t, pool)
	ok, improvement, size, err := h.Validate(t.Context(), rec, nil)
	if err != nil || ok || improvement != 0 || size != 0 {
		t.Fatalf("empty workload must produce no evidence: %t %f %d %v", ok, improvement, size, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, err := h.Validate(ctx, rec, bad); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	assertHypoPGSessionClean(t, pool)
}

func TestHypoPGSizeDoesNotAcquireAnotherSession(t *testing.T) {
	base := hypopgSessionPool(t)
	cfg := base.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = "hypopg_session_test,public"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h := NewHypoPG(pool, 1, noopLog2)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items (category)"}
	queries := []QueryInfo{{QueryID: 1,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}
	ok, improvement, size, err := h.Validate(ctx, rec, queries)
	if err != nil || !ok || improvement <= 1 || size <= 0 {
		t.Fatalf("single-session size: ok=%t improvement=%f size=%d err=%v",
			ok, improvement, size, err)
	}
	assertHypoPGSessionClean(t, pool)
}

func assertHypoPGSessionClean(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var timeout string
	if err := pool.QueryRow(t.Context(), "SHOW statement_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != "17s" {
		t.Fatalf("statement timeout leaked: %s", timeout)
	}
	var count int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM hypopg_session_test.hypopg_list_indexes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("hypothetical indexes leaked: %d", count)
	}
	var isolation string
	if err := pool.QueryRow(t.Context(), "SHOW transaction_isolation").Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(isolation, "read committed") {
		t.Fatal("transaction state leaked")
	}
}

func TestHypoPGConcurrentSessionsAndAvailability(t *testing.T) {
	base := hypopgSessionPool(t)
	cfg := base.Config().Copy()
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h := NewHypoPG(pool, 1, noopLog2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !h.IsAvailable(t.Context()) {
				t.Error("installed HypoPG not available")
				return
			}
			rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items (category)"}
			queries := []QueryInfo{{QueryID: 1,
				Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}
			ok, improvement, size, err := h.Validate(t.Context(), rec, queries)
			if err != nil || !ok || improvement <= 1 || size <= 0 {
				t.Errorf("concurrent session result: %t %f %d %v", ok, improvement, size, err)
			}
		}()
	}
	wg.Wait()
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatal("concurrent validation leaked a checkout")
	}
}

func TestHypoPGCleanupFailureDiscardsConnection(t *testing.T) {
	pool := hypopgSessionPool(t)
	session, err := openHypoPGSession(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := session.tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	admin := connectTestDB(t)
	defer admin.Close()
	var terminated bool
	err = admin.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&terminated)
	if err != nil || !terminated {
		t.Fatalf("terminate synthetic session: %t %v", terminated, err)
	}
	if err := session.close(); err == nil {
		t.Fatal("lost session cleanup silently succeeded")
	}
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatal("failed session was not discarded")
	}
	assertHypoPGSessionClean(t, pool)
}
