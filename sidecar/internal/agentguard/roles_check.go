package agentguard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Querier reads the catalog (a pool, a connection or a transaction).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DangerousRoles are the predefined roles an agent role may never be a
// member of, directly or through other roles (G1-01, AP-01).
var DangerousRoles = []string{"pg_execute_server_program", "pg_read_server_files",
	"pg_write_all_data", "pg_write_server_files"}

// RoleState is a role's Guard-relevant catalog state.
type RoleState struct {
	Name        string   `json:"name"`
	OID         uint32   `json:"oid"`
	Superuser   bool     `json:"superuser"`
	CreateRole  bool     `json:"createrole"`
	CreateDB    bool     `json:"createdb"`
	Replication bool     `json:"replication"`
	BypassRLS   bool     `json:"bypassrls"`
	CanLogin    bool     `json:"can_login"`
	ConnLimit   int      `json:"connection_limit"`
	MemberOf    []string `json:"member_of"`
	// Dangerous lists the DangerousRoles it is a member of, transitively.
	Dangerous []string `json:"dangerous"`
	// Owned counts objects it owns in any database of the cluster.
	Owned int `json:"owned"`
}

var roleStateSQL = `/* pg_sage guard_role_state v1 */
SELECT r.oid, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolreplication, r.rolbypassrls,
  r.rolcanlogin, r.rolconnlimit,
  ARRAY(SELECT g.rolname::text FROM pg_catalog.pg_auth_members m
        JOIN pg_catalog.pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid ORDER BY 1),
  ARRAY(SELECT d FROM unnest($2::text[]) AS d
        WHERE pg_catalog.pg_has_role(r.oid, d, 'MEMBER') ORDER BY 1),
  (SELECT count(*) FROM pg_catalog.pg_shdepend s
   WHERE s.refclassid = 'pg_catalog.pg_authid'::pg_catalog.regclass
     AND s.refobjid = r.oid AND s.deptype = 'o')::int
FROM pg_catalog.pg_roles r WHERE r.rolname = $1`

// ReadRoleState reads one role's state; a missing role is ErrNotFound.
func ReadRoleState(ctx context.Context, q Querier, rolname string) (RoleState, error) {
	st := RoleState{Name: rolname}
	err := q.QueryRow(ctx, roleStateSQL, rolname, DangerousRoles).Scan(&st.OID, &st.Superuser,
		&st.CreateRole, &st.CreateDB, &st.Replication, &st.BypassRLS, &st.CanLogin,
		&st.ConnLimit, &st.MemberOf, &st.Dangerous, &st.Owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleState{}, fmt.Errorf("%w: role %s", ErrNotFound, rolname)
	}
	if err != nil {
		return RoleState{}, fmt.Errorf("agentguard: reading role %s: %w", rolname, err)
	}
	return st, nil
}

// AgentViolations lists how an agent role breaks the §6.6 rules: no
// superuser, CREATEROLE, CREATEDB, REPLICATION or BYPASSRLS, no dangerous
// membership, owns nothing (G1-01, G1-07).
func (st RoleState) AgentViolations() []string {
	var out []string
	flag := func(on bool, what string) {
		if on {
			out = append(out, what)
		}
	}
	flag(st.Superuser, "is superuser")
	flag(st.BypassRLS, "has BYPASSRLS")
	flag(st.CreateRole, "has CREATEROLE")
	flag(st.CreateDB, "has CREATEDB")
	flag(st.Replication, "has REPLICATION")
	if len(st.Dangerous) > 0 {
		out = append(out, "is a member of "+strings.Join(st.Dangerous, ", "))
	}
	if st.Owned > 0 {
		out = append(out, fmt.Sprintf("owns %d objects", st.Owned))
	}
	return out
}

// postCheckEnsure verifies, inside the role transaction, that both roles
// equal the spec: attributes, no membership at all, owns nothing, the
// settings in each database and the broker's CONNECT.
func postCheckEnsure(ctx context.Context, tx pgx.Tx, cfg RoleConfig, req RoleRequest,
	version int, skipped []string) error {
	broker, login := BrokerRoleName(req.PrincipalID), LoginRoleName(req.PrincipalID)
	want := map[string]struct {
		login bool
		limit int
	}{broker: {true, cfg.BrokerConnectionLimit}, login: {false, cfg.ConnectionLimit}}
	for _, role := range []string{broker, login} {
		st, err := ReadRoleState(ctx, tx, role)
		if err != nil {
			return err
		}
		problems := st.AgentViolations()
		if st.CanLogin != want[role].login || st.ConnLimit != want[role].limit {
			problems = append(problems, fmt.Sprintf("login %v limit %d", st.CanLogin,
				st.ConnLimit))
		}
		if len(st.MemberOf) > 0 {
			problems = append(problems, "is a member of "+strings.Join(st.MemberOf, ", "))
		}
		if len(problems) > 0 {
			return fmt.Errorf("%w: %s %s", ErrPostCheck, role, strings.Join(problems, "; "))
		}
	}
	return postCheckSettings(ctx, tx, cfg, req, version, skipped)
}

var roleSettingsSQL = `/* pg_sage guard_role_state v1 */
SELECT COALESCE((SELECT s.setconfig FROM pg_catalog.pg_db_role_setting s
  JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
  JOIN pg_catalog.pg_roles r ON r.oid = s.setrole
  WHERE r.rolname = $1 AND d.datname = $2), '{}'::text[]),
  pg_catalog.has_database_privilege($1, $2, 'CONNECT')`

func postCheckSettings(ctx context.Context, tx pgx.Tx, cfg RoleConfig, req RoleRequest,
	version int, skipped []string) error {
	broker, login := BrokerRoleName(req.PrincipalID), LoginRoleName(req.PrincipalID)
	for _, d := range req.Cluster.Databases {
		for _, role := range []string{broker, login} {
			var conf []string
			var connect bool
			if err := tx.QueryRow(ctx, roleSettingsSQL, role, d.Name).Scan(&conf,
				&connect); err != nil {
				return fmt.Errorf("agentguard: reading settings of %s in %s: %w", role,
					d.Name, err)
			}
			want := wantedSettings(cfg, version, skipped, role == login)
			for _, w := range want {
				if !slices.Contains(conf, w) {
					return fmt.Errorf("%w: %s in %s lacks %s", ErrPostCheck, role, d.Name, w)
				}
			}
			if role == broker && !connect {
				return fmt.Errorf("%w: %s cannot connect to %s", ErrPostCheck, role, d.Name)
			}
		}
	}
	return nil
}

func wantedSettings(cfg RoleConfig, version int, skipped []string, login bool) []string {
	var out []string
	for _, s := range cfg.Settings(version) {
		if !slices.Contains(skipped, s.Name) {
			out = append(out, s.Name+"="+s.Value)
		}
	}
	if login {
		out = append(out, "default_transaction_read_only=on")
	}
	return out
}
