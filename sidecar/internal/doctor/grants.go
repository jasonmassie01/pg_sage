package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rolegrants"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// fixSeparator joins the statements of a fix that needs several.
const fixSeparator = ";\n"

// roleSQL reads the connected role, whether it is a superuser, and whether
// it holds pg_monitor's privileges (USAGE: inherited, so a NOINHERIT
// member is not counted).
const roleSQL = `/* pg_sage doctor */ SELECT current_user::text, r.rolsuper,
  pg_catalog.pg_has_role(current_user, 'pg_monitor', 'USAGE')
  FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

// roleCheck fails without pg_monitor (pg_sage reads the statistics views
// through it) and warns for each grant that only limits actions. Fix
// lists every missing GRANT, built by rolegrants like the startup checks.
func roleCheck(ctx context.Context, pool *pgxpool.Pool) Check {
	c := Check{ID: IDRoleGrants, Title: titleRole}
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var name string
	var super, monitor bool
	if err := pool.QueryRow(qctx, roleSQL).Scan(&name, &super, &monitor); err != nil {
		c.Status, c.Detail = StatusUnknown, "could not read the role: "+err.Error()
		return c
	}
	role := sqlident.Ident(name)
	if super {
		c.Status, c.Detail = StatusPass, "role "+role+" is a superuser"
		return c
	}
	var missing, fixes []string
	if !monitor {
		missing = append(missing, "pg_monitor (the statistics views pg_sage reads)")
		fixes = append(fixes, "GRANT pg_monitor TO "+role)
	}
	limits, limitFixes := limitingGrants(qctx, pool, role)
	missing, fixes = append(missing, limits...), append(fixes, limitFixes...)
	c.Fix = strings.Join(fixes, fixSeparator)
	switch {
	case !monitor:
		c.Status = StatusFail
	case len(missing) > 0:
		c.Status = StatusWarn
	default:
		c.Status = StatusPass
		c.Detail = "role " + role + " holds pg_monitor and every grant pg_sage uses"
		return c
	}
	c.Detail = "role " + role + " lacks " + strings.Join(missing, "; ")
	return c
}

// limitingGrants are the grants whose absence limits actions, not
// monitoring: CREATE on table schemas, SELECT on sequences and
// pg_signal_backend. A failed read is reported as missing knowledge.
func limitingGrants(ctx context.Context, pool *pgxpool.Pool, role string) ([]string,
	[]string) {
	var missing, fixes []string
	if s, err := rolegrants.CheckSchemaCreate(ctx, pool); err != nil {
		missing = append(missing, "a readable schema list ("+err.Error()+")")
	} else if s.Lacking() {
		missing = append(missing, s.What()+" ("+rolegrants.Need+")")
		fixes = append(fixes, s.GrantSQL(role))
	}
	if s, err := rolegrants.CheckSequenceSelect(ctx, pool); err != nil {
		missing = append(missing, "a readable sequence list ("+err.Error()+")")
	} else if s.Lacking() {
		missing = append(missing, s.What()+" ("+rolegrants.SequenceNeed+")")
		fixes = append(fixes, s.GrantSQL(role))
	}
	if s, err := rolegrants.CheckSignalBackend(ctx, pool); err != nil {
		missing = append(missing, "a readable pg_signal_backend ("+err.Error()+")")
	} else if !s.Usable {
		missing = append(missing, "pg_signal_backend ("+rolegrants.SignalNeed+")")
		fixes = append(fixes, s.GrantSQL(role))
	}
	return missing, fixes
}

// extensionsSQL reads pg_stat_statements' schema (NULL when absent) and
// whether HypoPG is installed or installable, from the catalogs only.
const extensionsSQL = `/* pg_sage doctor */ SELECT
  (SELECT n.nspname::text FROM pg_catalog.pg_extension e
     JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
    WHERE e.extname = 'pg_stat_statements'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'hypopg'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'hypopg')`

// extensionsCheck fails when pg_stat_statements is missing or unreadable
// and warns when HypoPG is missing, saying what that limits.
func extensionsCheck(ctx context.Context, pool *pgxpool.Pool, p Platform) Check {
	c := Check{ID: IDExtensions, Title: titleExtensions}
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var schema *string
	var hypo, hypoAvailable bool
	if err := pool.QueryRow(qctx, extensionsSQL).Scan(&schema, &hypo,
		&hypoAvailable); err != nil {
		c.Status, c.Detail = StatusUnknown, "could not read the extensions: "+err.Error()
		return c
	}
	if schema == nil {
		c.Status = StatusFail
		c.Detail = "pg_stat_statements is not installed: no query statistics"
		c.Fix = "CREATE EXTENSION pg_stat_statements;\n" + preloadFix(p)
		return c
	}
	rel := sqlident.Ident(*schema) + ".pg_stat_statements"
	var n int64
	if err := pool.QueryRow(qctx, "/* pg_sage doctor */ SELECT count(*) FROM "+rel).
		Scan(&n); err != nil {
		c.Status = StatusFail
		c.Detail = "cannot read " + rel + ": " + err.Error()
		c.Fix = preloadFix(p)
		return c
	}
	if !hypo {
		c.Status = StatusWarn
		c.Detail = "pg_stat_statements is readable; HypoPG is not installed, so index " +
			"proposals are not cost-checked on hypothetical indexes and carry less " +
			"confidence"
		c.Fix = "CREATE EXTENSION hypopg"
		if !hypoAvailable {
			c.Fix = "install the hypopg package on the server, then CREATE EXTENSION hypopg"
		}
		return c
	}
	c.Status = StatusPass
	c.Detail = fmt.Sprintf("pg_stat_statements is readable (%d statements); HypoPG is "+
		"installed", n)
	return c
}

// preloadFix is how this platform loads pg_stat_statements at server start.
func preloadFix(p Platform) string {
	switch p.Code {
	case "RDS", "AUR":
		return "add pg_stat_statements to shared_preload_libraries in the DB parameter " +
			"group, then reboot the instance"
	case "AZ":
		return "add pg_stat_statements to the shared_preload_libraries server parameter, " +
			"then restart the server"
	case "GCP", "NEON", "SUPA", "LB":
		return "the provider preloads pg_stat_statements; CREATE EXTENSION is enough"
	}
	return "ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements' (keep any " +
		"libraries already listed), then restart PostgreSQL"
}

// queryTextCheck is the pg_read_all_stats test pg_stat_statements makes.
func queryTextCheck(ctx context.Context, pool *pgxpool.Pool) Check {
	c := Check{ID: IDQueryText, Title: titleQueryText}
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var visible bool
	var role string
	err := pool.QueryRow(qctx, "/* pg_sage doctor */ SELECT "+
		rolegrants.QueryTextVisibleSQL+", current_user::text").Scan(&visible, &role)
	if err != nil {
		c.Status, c.Detail = StatusUnknown, "could not check pg_read_all_stats: "+err.Error()
		return c
	}
	if visible {
		c.Status, c.Detail = StatusPass, "other roles' query text is visible"
		return c
	}
	c.Status = StatusWarn
	c.Detail = "other roles' query text reads <insufficient privilege>: the role does " +
		"not inherit pg_read_all_stats, so findings cannot name their queries"
	c.Fix = "GRANT pg_read_all_stats TO " + sqlident.Ident(role) + " (or GRANT pg_monitor; " +
		"keep the role INHERIT)"
	return c
}
