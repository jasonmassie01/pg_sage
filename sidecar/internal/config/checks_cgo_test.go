//go:build cgo

package config

import (
	"strings"
	"testing"
)

// The parser-backed refusals (market-11). They need libpg_query, so they
// run only in a cgo build (CI and release builds).

func TestChecks_SQLRefusedAtLoad(t *testing.T) {
	cases := map[string]string{
		"insert":       "INSERT INTO t VALUES (1)",
		"two":          "SELECT 1 AS x; SELECT 2 AS x",
		"for update":   "SELECT relname FROM pg_class FOR UPDATE",
		"cte delete":   "WITH d AS (DELETE FROM t RETURNING id) SELECT id AS x FROM d",
		"pg_sleep":     "SELECT pg_sleep(1) AS x",
		"dblink":       "SELECT * FROM dblink('x', 'SELECT 1') AS t(x int)",
		"pg_read_file": "SELECT pg_read_file('/etc/passwd') AS x",
		"lo_import":    "SELECT lo_import('/tmp/x') AS x",
		"parameter":    "SELECT relname AS x FROM pg_class WHERE oid = $1",
		"select into":  "SELECT 1 AS x INTO t2",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCheck()
			c.SQL, c.ObjectColumn = sql, "x"
			if _, err := PrepareCheck(c); err == nil {
				t.Fatalf("PrepareCheck(%q) accepted a statement it must refuse", sql)
			} else if !strings.Contains(err.Error(), "table_without_pk") {
				t.Fatalf("error %q does not name the check", err)
			}
		})
	}
}

func TestChecks_PrepareCanonicalizesAndListsFunctions(t *testing.T) {
	c := validCheck()
	c.SQL = "select c.relname, now() as at from pg_class c -- comment"
	p, err := PrepareCheck(c)
	if err != nil {
		t.Fatalf("PrepareCheck: %v", err)
	}
	if strings.Contains(p.Canonical, "comment") || p.Fingerprint == "" {
		t.Fatalf("canonical = %q fingerprint = %q", p.Canonical, p.Fingerprint)
	}
	found := false
	for _, f := range p.Functions {
		found = found || f == "now"
	}
	if !found {
		t.Fatalf("Functions = %v, want now listed", p.Functions)
	}
}
