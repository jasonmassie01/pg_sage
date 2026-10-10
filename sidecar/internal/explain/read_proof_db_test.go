//go:build cgo

package explain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The brokered read proof (spec §6.8 S3) is the analyze guard with three
// changes: row-level security is allowed unless a policy calls a volatile
// function, SECURITY DEFINER functions are refused, and an extra name
// deny-list and a catalog allowlist apply.

const proofFixtureSQL = `
DROP SCHEMA IF EXISTS rp_fx CASCADE;
CREATE SCHEMA rp_fx;
CREATE TABLE rp_fx.plain (id int, owner_name text);
INSERT INTO rp_fx.plain VALUES (1, 'a');
CREATE FUNCTION rp_fx.stable_fn() RETURNS int LANGUAGE sql STABLE AS 'SELECT 1';
CREATE FUNCTION rp_fx.volatile_fn() RETURNS int LANGUAGE sql VOLATILE AS 'SELECT 1';
CREATE FUNCTION rp_fx.definer_fn() RETURNS int LANGUAGE sql STABLE SECURITY DEFINER
	AS 'SELECT 1';
CREATE TABLE rp_fx.rls_stable (id int, owner_name text);
ALTER TABLE rp_fx.rls_stable ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON rp_fx.rls_stable USING (owner_name = current_user);
CREATE TABLE rp_fx.rls_volatile (id int);
ALTER TABLE rp_fx.rls_volatile ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON rp_fx.rls_volatile USING (id < rp_fx.volatile_fn());
CREATE TABLE rp_fx.rls_builtin_volatile (id int);
ALTER TABLE rp_fx.rls_builtin_volatile ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON rp_fx.rls_builtin_volatile USING (random() < 2);
CREATE VIEW rp_fx.over_rls AS SELECT id FROM rp_fx.rls_volatile;
`

const proofCleanupSQL = `DROP SCHEMA IF EXISTS rp_fx CASCADE`

func brokerProofOptions() ReadProofOptions {
	return ReadProofOptions{
		AllowRLS:            true,
		DenySecurityDefiner: true,
		DenyFunction: func(n sqlast.QualifiedName) bool {
			return n.Name == "set_config" || n.Schema == "sage"
		},
		AllowCatalogRelation: func(schema, name string) bool {
			return schema == "pg_catalog" && (name == "pg_class" || name == "pg_namespace")
		},
	}
}

func TestProveReadWithBrokerOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := liveExplainPool(t)
	if _, err := pool.Exec(ctx, proofFixtureSQL); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), proofCleanupSQL) })

	cases := []struct {
		sql, refusal string // "" = proven
	}{
		{"SELECT id FROM rp_fx.plain", ""},
		{"SELECT rp_fx.stable_fn()", ""},
		{"SELECT id FROM rp_fx.rls_stable", ""},
		{"SELECT relname FROM pg_class WHERE relname = 'x'", ""},
		{"SELECT n.nspname FROM pg_catalog.pg_namespace n", ""},
		{"SELECT rp_fx.volatile_fn()", "volatile function"},
		{"SELECT rp_fx.definer_fn()", "SECURITY DEFINER"},
		{"SELECT set_config('a.b', 'c', true)", "set_config"},
		{"SELECT id FROM rp_fx.rls_volatile", "row-level security policy"},
		{"SELECT id FROM rp_fx.rls_builtin_volatile", "row-level security policy"},
		{"SELECT id FROM rp_fx.over_rls", "row-level security policy"},
		{"SELECT proname FROM pg_proc", "pg_catalog.pg_proc"},
		{"SELECT rolname FROM pg_catalog.pg_roles", "pg_catalog.pg_roles"},
		{"SELECT * FROM rp_fx.missing", "unknown relation"},
	}
	for _, c := range cases {
		got, err := ProveRead(ctx, pool, c.sql, nil, brokerProofOptions())
		if err != nil {
			t.Errorf("%q: error = %v", c.sql, err)
			continue
		}
		switch {
		case c.refusal == "" && got != "":
			t.Errorf("%q: refused (%s), want proven", c.sql, got)
		case c.refusal != "" && !strings.Contains(got, c.refusal):
			t.Errorf("%q: refusal = %q, want it to mention %q", c.sql, got, c.refusal)
		}
	}
}

// With zero options ProveRead is exactly the analyze guard: any row-level
// security refuses, and a definer function that is stable passes.
func TestProveReadZeroOptionsMatchesAnalyzeGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := liveExplainPool(t)
	if _, err := pool.Exec(ctx, proofFixtureSQL); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), proofCleanupSQL) })

	got, err := ProveRead(ctx, pool, "SELECT id FROM rp_fx.rls_stable", nil,
		ReadProofOptions{})
	if err != nil || !strings.Contains(got, "row-level security") {
		t.Errorf("rls with zero options = %q, %v; want a row-level security refusal",
			got, err)
	}
	got, err = ProveRead(ctx, pool, "SELECT rp_fx.definer_fn()", nil, ReadProofOptions{})
	if err != nil || got != "" {
		t.Errorf("stable definer with zero options = %q, %v; want proven", got, err)
	}
	got, err = ProveRead(ctx, pool, "SELECT proname FROM pg_proc", nil, ReadProofOptions{})
	if err != nil || got != "" {
		t.Errorf("catalog read with zero options = %q, %v; want proven", got, err)
	}
}
