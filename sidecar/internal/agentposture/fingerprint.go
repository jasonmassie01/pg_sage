package agentposture

import (
	"context"
	"fmt"
)

// fingerprintParts are the per-catalog sums, added in one statement.
var fingerprintParts = []string{
	`SELECT COALESCE(sum(hashtext(concat_ws('#', r.oid, r.rolname, r.rolsuper,
    r.rolinherit, r.rolcreaterole, r.rolcreatedb, r.rolcanlogin, r.rolreplication,
    r.rolbypassrls))), 0) FROM pg_catalog.pg_roles r`,
	`SELECT COALESCE(sum(hashtext(m::text)), 0) FROM pg_catalog.pg_auth_members m`,
	`SELECT COALESCE(sum(hashtext(s::text)), 0) FROM pg_catalog.pg_db_role_setting s`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', n.oid, n.nspname, n.nspowner,
    n.nspacl))), 0) FROM pg_catalog.pg_namespace n`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', c.oid, c.relname, c.relnamespace,
    c.relkind, c.relowner, c.relacl, c.relrowsecurity, c.relforcerowsecurity,
    c.reloptions))), 0) FROM pg_catalog.pg_class c
    WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')`,
	`SELECT COALESCE(sum(hashtext(p::text)), 0) FROM pg_catalog.pg_policy p`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', w.oid, w.ev_class, w.xmin))), 0)
    FROM pg_catalog.pg_rewrite w`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', f.oid, f.proname, f.proowner,
    f.prosecdef, f.proacl, f.proconfig))), 0) FROM pg_catalog.pg_proc f
    WHERE f.prosecdef OR f.proacl IS NOT NULL OR f.proconfig IS NOT NULL`,
	`SELECT COALESCE(sum(hashtext(d::text)), 0) FROM pg_catalog.pg_default_acl d`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', l.oid, l.lanpltrusted, l.lanacl))), 0)
    FROM pg_catalog.pg_language l`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', w.oid, w.fdwowner, w.fdwacl))), 0)
    FROM pg_catalog.pg_foreign_data_wrapper w`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', s.oid, s.srvowner, s.srvacl))), 0)
    FROM pg_catalog.pg_foreign_server s`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', e.oid, e.extname, e.extversion))), 0)
    FROM pg_catalog.pg_extension e`,
	`SELECT COALESCE(sum(hashtext(concat_ws('#', d.oid, d.datacl))), 0)
    FROM pg_catalog.pg_database d WHERE d.datname = current_database()`,
}

// fingerprintSQL is Guard's own hash of the catalog rows posture reads
// (spec §6.15): roles and memberships, per-role settings, schema,
// relation, function, language, foreign-data, database and default ACLs,
// ownership, row-level security and its policies, view rules and
// extension versions. Each catalog adds an order-independent sum of row
// hashes; it reads no statistics, so data changes, ANALYZE and VACUUM
// leave it alone. Each sum stays far below 2^63 (int4 hashes over at most
// millions of rows), so the bigint total cannot overflow.
var fingerprintSQL = func() string {
	sql := "SELECT 0::bigint"
	for _, p := range fingerprintParts {
		sql += "\n + (" + p + ")"
	}
	return Statement("fingerprint", sql)
}()

// Fingerprint hashes the catalog rows posture reads; it changes when any
// of them changes.
func Fingerprint(ctx context.Context, q Querier) (int64, error) {
	var fp int64
	if err := q.QueryRow(ctx, fingerprintSQL).Scan(&fp); err != nil {
		return 0, fmt.Errorf("agent posture: catalog fingerprint: %w", err)
	}
	return fp, nil
}
