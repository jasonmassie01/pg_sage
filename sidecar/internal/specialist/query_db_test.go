package specialist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// query_hash resolution against a real pg_stat_statements, and the
// caller's query scope persisted on its request record.

func TestPGStore_QueryScopeRoundTrip(t *testing.T) {
	pool := livePool(t)
	st := NewPGStore(pool)
	ctx := context.Background()
	tok := uniqueToken("tokq")
	scope := &QueryScope{QueryID: "-9223372036854775808", QueryHash: queryHashOK,
		Applied: QueryAppliedProbes}
	if _, err := st.Record(ctx, Record{Kind: KindOpen, TokenID: tok, Actor: "agent:" + tok,
		Transport: "http", Database: "orders", InvestigationID: string(inv),
		Created: true, Query: scope}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ForInvestigation(ctx, tok, "orders", string(inv))
	if err != nil || got == nil || got.Query == nil || *got.Query != *scope {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	plain := uniqueToken("tokp")
	if _, err := st.Record(ctx, Record{Kind: KindOpen, TokenID: plain, Actor: "a",
		Transport: "http", Database: "orders", InvestigationID: string(inv)}); err != nil {
		t.Fatal(err)
	}
	got, err = st.ForInvestigation(ctx, plain, "orders", string(inv))
	if err != nil || got == nil || got.Query != nil {
		t.Fatalf("a v1 record has no scope: %+v %v", got, err)
	}
	var stored *string
	if err := pool.QueryRow(ctx, `SELECT query_scope::text FROM sage.specialist_requests
		WHERE token_id = $1`, plain).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("no scope is stored as NULL: %v %v", stored, err)
	}
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func TestResolveQueryHash_FindsTheStatement(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS specmap_hash_probe (a int)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS specmap_hash_probe`)
	})
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM specmap_hash_probe WHERE a = $1`,
		7).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var qid int64
	var text string
	if err := pool.QueryRow(ctx, `SELECT queryid, query FROM pg_stat_statements
		WHERE query LIKE 'SELECT count(*) FROM specmap_hash_probe%' LIMIT 1`).Scan(&qid,
		&text); err != nil {
		t.Fatalf("the statement is not in pg_stat_statements: %v", err)
	}
	got, err := ResolveQueryHash(ctx, pool, hashOf(text))
	if err != nil || len(got) != 1 || got[0] != qid {
		t.Fatalf("resolve %q: %v %v, want [%d]", text, got, err, qid)
	}
	for _, h := range []string{strings.Repeat("0", 64), "' OR 1=1 --",
		hashOf(text) + "' OR '1'='1"} {
		got, err := ResolveQueryHash(ctx, pool, h)
		if err != nil || len(got) != 0 {
			t.Fatalf("%q matches nothing (a parameter, never SQL): %v %v", h, got, err)
		}
	}
}

func TestResolveQueryHash_Unavailable(t *testing.T) {
	if _, err := ResolveQueryHash(context.Background(), nil, queryHashOK); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("no pool: %v", err)
	}
	pool := livePool(t)
	ctx := context.Background()
	const db = "specmap_no_pss"
	_, _ = pool.Exec(ctx, `DROP DATABASE IF EXISTS `+db)
	if _, err := pool.Exec(ctx, `CREATE DATABASE `+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+db+
			` WITH (FORCE)`)
	})
	u, err := url.Parse(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	other, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = ResolveQueryHash(ctx, other, queryHashOK)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "pg_stat_statements") {
		t.Fatalf("without pg_stat_statements: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ResolveQueryHash(cancelled, pool, queryHashOK); err == nil {
		t.Fatal("a cancelled context must fail")
	}
}
