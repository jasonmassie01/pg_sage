package rvfixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Reader configures the login role ReaderPool creates: what pg_sage's
// monitoring role is granted on this database.
type Reader struct {
	// ReadAllStats grants pg_read_all_stats (pg_stat_statements text of
	// every user's statements).
	ReadAllStats bool
	// SelectTables are the rv tables ("rv_orders") the role may SELECT.
	SelectTables []string
}

// ReaderPool creates a login role for this test with r's grants, plus
// what pg_sage always has on its own schema (sage: all), and returns a
// pool connected as it. The role is dropped after the fixture database.
func (f *Fixture) ReaderPool(r Reader) (*pgxpool.Pool, string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	role, password := f.Name+"_r", randomHex(12)
	ident := pgx.Identifier{role}.Sanitize()
	stmts := []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", ident, password),
		"GRANT USAGE ON SCHEMA rv TO " + ident,
		"GRANT ALL ON SCHEMA sage TO " + ident,
		"GRANT ALL ON ALL TABLES IN SCHEMA sage TO " + ident,
		"GRANT ALL ON ALL SEQUENCES IN SCHEMA sage TO " + ident,
		"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA sage TO " + ident,
	}
	if r.ReadAllStats {
		stmts = append(stmts, "GRANT pg_read_all_stats TO "+ident)
	}
	for _, table := range r.SelectTables {
		stmts = append(stmts, "GRANT SELECT ON "+
			pgx.Identifier{Schema, table}.Sanitize()+" TO "+ident)
	}
	f.t.Cleanup(func() { f.dropRole(ident) })
	for _, stmt := range stmts {
		if _, err := f.Pool.Exec(ctx, stmt); err != nil {
			f.t.Fatalf("reader role: %v", err)
		}
	}
	dsn, err := withUser(f.DSN, role, password)
	if err != nil {
		f.t.Fatalf("reader DSN: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		f.t.Fatalf("connect as reader: %v", err)
	}
	f.t.Cleanup(pool.Close)
	return pool, role
}

// dropRole revokes the role's privileges in the fixture database and drops
// it. Cleanups run in reverse order, so the reader pool is closed first
// and the fixture pool is still open.
func (f *Fixture) dropRole(ident string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, stmt := range []string{"DROP OWNED BY " + ident, "DROP ROLE " + ident} {
		if _, err := f.Pool.Exec(ctx, stmt); err != nil {
			f.t.Errorf("drop reader role: %s: %v", stmt, err)
			return
		}
	}
}

func withUser(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("random: %v", err))
	}
	return hex.EncodeToString(b)
}
