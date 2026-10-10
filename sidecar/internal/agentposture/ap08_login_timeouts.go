package agentposture

import (
	"context"
	"fmt"
	"strings"
)

func init() { Register(ap08{}) }

// ap08 reports agent and application login roles that run without a
// statement_timeout or an idle_in_transaction_session_timeout: a stuck
// agent session then holds locks and snapshots for as long as it likes.
// A timeout counts when it is set (non-zero) for the role in this database
// or in all databases, for the whole database, or in the server's
// configuration where the session can see it. Superusers, pg_sage's own
// role, reserved pg_ roles and provider administration roles are skipped.
type ap08 struct{}

func (ap08) Spec() Spec {
	return Spec{ID: "AP-08", Title: "Login roles without session timeouts",
		Severity: Info}
}

// providerAdminRoles are managed-service administration logins.
const providerAdminRoles = `^(rdsadmin|rdsrepladmin|rds_|cloudsqladmin|cloudsqlreplica|` +
	`azure_superuser|azuresu|alloydbadmin|alloydbreplica)`

// ap08SQL reports, per candidate role, whether each timeout is covered.
var ap08SQL = Statement("AP-08", `WITH db AS (
  SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database()
), server AS (
  SELECT name FROM pg_catalog.pg_settings
  WHERE name IN ('statement_timeout', 'idle_in_transaction_session_timeout')
    AND setting <> '0'
    AND source IN ('configuration file', 'command line', 'environment variable', 'global')
), roleset AS (
  SELECT s.setrole, split_part(c, '=', 1) AS name
  FROM pg_catalog.pg_db_role_setting s, db, pg_catalog.unnest(s.setconfig) c
  WHERE (s.setdatabase = 0 OR s.setdatabase = db.oid)
    AND split_part(c, '=', 1) IN ('statement_timeout', 'idle_in_transaction_session_timeout')
    AND split_part(c, '=', 2) !~ '^0+(us|ms|s|min|h|d)?$'
    AND NOT (s.setrole = 0 AND s.setdatabase = 0)
)
SELECT r.oid, r.rolname::text,
  EXISTS (SELECT 1 FROM roleset WHERE roleset.name = 'statement_timeout'
          AND roleset.setrole IN (0, r.oid))
    OR EXISTS (SELECT 1 FROM server WHERE server.name = 'statement_timeout'),
  EXISTS (SELECT 1 FROM roleset WHERE roleset.name = 'idle_in_transaction_session_timeout'
          AND roleset.setrole IN (0, r.oid))
    OR EXISTS (SELECT 1 FROM server
               WHERE server.name = 'idle_in_transaction_session_timeout')
FROM pg_catalog.pg_roles r
WHERE r.rolcanlogin AND NOT r.rolsuper AND r.oid <> $1
  AND r.rolname !~ '^pg_' AND r.rolname !~ $2
ORDER BY r.rolname
LIMIT $3`)

// Timeouts the fix script sets (spec §9 agents.roles defaults).
const (
	fixStatementTimeout = "30s"
	fixIdleTimeout      = "60s"
)

func (ap08) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap08SQL, in.Env.Self.OID, providerAdminRoles, maxRows)
	if err != nil {
		return nil, fmt.Errorf("read login role timeouts: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var oid uint32
		var name string
		var stmt, idle bool
		if err := rows.Scan(&oid, &name, &stmt, &idle); err != nil {
			return nil, fmt.Errorf("read login role timeouts: %w", err)
		}
		if stmt && idle {
			continue
		}
		agent, isAgent := in.Env.Agent(oid)
		out = append(out, ap08Finding(name, stmt, idle, isAgent && agent.OID == oid))
	}
	return out, rows.Err()
}

func ap08Finding(name string, stmt, idle, agent bool) Finding {
	var missing, fix []string
	role := QuoteIdent(name)
	if !stmt {
		missing = append(missing, "statement_timeout")
		fix = append(fix, fmt.Sprintf("ALTER ROLE %s SET statement_timeout = '%s';", role,
			fixStatementTimeout))
	}
	if !idle {
		missing = append(missing, "idle_in_transaction_session_timeout")
		fix = append(fix, fmt.Sprintf("ALTER ROLE %s SET "+
			"idle_in_transaction_session_timeout = '%s';", role, fixIdleTimeout))
	}
	kind := "Login role"
	if agent {
		kind = "Agent role"
	}
	return Finding{Severity: Info, ObjectType: "role", Object: name,
		Title: fmt.Sprintf("%s %s has no %s", kind, name, joinAnd(missing)),
		Detail: fmt.Sprintf("%s %s runs without %s, so a stuck or runaway session holds "+
			"its locks and snapshot until someone ends it.", kind, name, joinAnd(missing)),
		Recommendation: "Set session timeouts on the role that bound what one session " +
			"can hold.",
		FixScript: strings.Join(fix, "\n"),
		Caveat: "Pick values the role's longest legitimate statement fits in. A " +
			"server-wide setting pg_sage's session overrides is not visible here.",
		Evidence: []Evidence{{Source: "pg_db_role_setting", Ref: name,
			Detail: "not set: " + strings.Join(missing, ", ")}}}
}
