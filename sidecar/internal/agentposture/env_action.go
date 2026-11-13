package agentposture

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var actionRoleSQL = Statement("env", `SELECT r.oid FROM pg_catalog.pg_roles r
WHERE r.rolname = $1`)

// resolveAction reads the action identity's role. A role that does not
// exist, or is pg_sage's own, is no separate identity.
func resolveAction(ctx context.Context, q Querier, name string, env *Env) error {
	if name == "" || name == env.Self.Name {
		return nil
	}
	var oid uint32
	err := q.QueryRow(ctx, actionRoleSQL, name).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent posture: read the action role %q: %w", name, err)
	}
	env.Action = &Role{OID: oid, Name: name, Source: SourceAction}
	return nil
}
