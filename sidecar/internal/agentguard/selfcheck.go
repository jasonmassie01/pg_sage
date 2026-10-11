package agentguard

import (
	"context"
	"fmt"
	"strings"
)

// SelfResult is the startup self-check of pg_sage's own role for Guard
// features (G1-01, AP-14): it must not be superuser, must not hold
// BYPASSRLS, must not inherit an agent role's privileges, and needs
// CREATEROLE to manage agent roles.
type SelfResult struct {
	Role       string   `json:"role"`
	Superuser  bool     `json:"superuser"`
	BypassRLS  bool     `json:"bypassrls"`
	CreateRole bool     `json:"createrole"`
	Inherits   []string `json:"inherits_agent_roles"`
}

// Problems lists every failed condition, in a fixed order.
func (r SelfResult) Problems() []string {
	var out []string
	if r.Superuser {
		out = append(out, "is superuser (Guard needs a non-superuser role with CREATEROLE)")
	}
	if r.BypassRLS {
		out = append(out, "holds BYPASSRLS")
	}
	if !r.CreateRole {
		out = append(out, "lacks CREATEROLE")
	}
	if len(r.Inherits) > 0 {
		out = append(out, "inherits agent roles "+strings.Join(r.Inherits, ", "))
	}
	return out
}

// Err is nil when the role passes, else ErrSelfCheck naming the problems
// and the role.
func (r SelfResult) Err() error {
	p := r.Problems()
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%w: role %s %s", ErrSelfCheck, r.Role, strings.Join(p, "; "))
}

// selfSQL reads pg_sage's own role. A superuser "has" every role, so its
// inherited list is not meaningful and is skipped.
const selfSQL = `/* pg_sage guard_self_check v1 */
SELECT r.rolname::text, r.rolsuper, r.rolbypassrls, r.rolcreaterole,
  CASE WHEN r.rolsuper THEN '{}'::text[] ELSE ARRAY(
    SELECT a.rolname::text FROM pg_catalog.pg_roles a
    WHERE a.rolname ~ '` + RoleRegex + `'
      AND pg_catalog.pg_has_role(r.oid, a.oid, 'USAGE') ORDER BY 1) END
FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

// SelfCheck reads pg_sage's own role on q's connection.
func SelfCheck(ctx context.Context, q Querier) (SelfResult, error) {
	var r SelfResult
	err := q.QueryRow(ctx, selfSQL).Scan(&r.Role, &r.Superuser, &r.BypassRLS,
		&r.CreateRole, &r.Inherits)
	if err != nil {
		return SelfResult{}, fmt.Errorf("agentguard: self-check of pg_sage's role: %w", err)
	}
	return r, nil
}
