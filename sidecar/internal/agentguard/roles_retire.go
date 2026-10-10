package agentguard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Retire runs guard_role_retire. It refuses up front when another
// grantor's privileges remain (pg_sage cannot revoke them). Otherwise, in
// every database of the cluster, it drops what the roles own and revokes
// what pg_sage granted them (DROP OWNED BY, under a temporary INHERIT on
// the roles), then drops both roles and marks them retired. It is
// idempotent: absent roles are skipped, and the post-check requires them
// absent from the cluster.
func (m *RoleManager) Retire(ctx context.Context, req RoleRequest) (RoleResult, error) {
	if err := req.validate(); err != nil {
		return RoleResult{}, err
	}
	v, err := serverVersion(ctx, req.Cluster.Admin)
	if err != nil {
		return RoleResult{}, err
	}
	if v < MinRoleServerVersion {
		return RoleResult{}, fmt.Errorf("%w: server version %d", ErrRoleManagementUnsupported,
			v)
	}
	gateReq, err := policyRequest(executor.ActionTypeGuardRoleRetire, req)
	if err != nil {
		return RoleResult{}, err
	}
	run := &retireRun{m: m, req: req}
	run.result.LoginRole = LoginRoleName(req.PrincipalID)
	run.result.BrokerRole = BrokerRoleName(req.PrincipalID)
	actionID, err := req.Executor.Apply(ctx, executor.ActionIntent{Request: gateReq,
		Authorize: authorizer(req.Executor, gateReq), SlotHeld: true, Execute: run.execute})
	run.result.ActionID = actionID
	return run.result, err
}

type retireRun struct {
	m          *RoleManager
	req        RoleRequest
	result     RoleResult
	statements []string
}

func (r *retireRun) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	roles := []string{r.result.BrokerRole, r.result.LoginRole}
	if err := r.refuseResidue(ctx, roles); err != nil {
		return 0, err
	}
	if err := r.adminTx(ctx, roles, r.inherit(true)); err != nil {
		return 0, err
	}
	for _, d := range r.req.Cluster.Databases {
		if err := r.dropOwned(ctx, d, roles); err != nil {
			r.restoreMembership(ctx, roles)
			return 0, err
		}
	}
	if err := r.adminTx(ctx, roles, r.dropRoles); err != nil {
		r.restoreMembership(ctx, roles)
		return 0, err
	}
	err := r.m.store.SetClusterRoleStatus(ctx, r.req.PrincipalID, r.req.Cluster.Key,
		RoleStatusRetired, nil)
	if err != nil && !isNotFound(err) {
		return 0, fmt.Errorf("roles of %s were dropped but the registry was not "+
			"updated: %w", r.req.PrincipalID, err)
	}
	return recordRoleAction(ctx, r.req, decision, executor.ActionTypeGuardRoleRetire,
		r.statements, map[string]any{"retired": true})
}

// refuseResidue refuses the retire before changing anything when another
// grantor's privileges remain: pg_sage could not revoke them, and DROP
// ROLE would fail half-way.
func (r *retireRun) refuseResidue(ctx context.Context, roles []string) error {
	var found []string
	for _, d := range r.req.Cluster.Databases {
		res, err := ForeignGrants(ctx, d.Pool, roles)
		if err != nil {
			return err
		}
		for _, x := range res {
			found = append(found, d.Name+": "+x.String())
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("%w: privileges from another grantor remain (%s); their "+
			"grantor must revoke them", ErrPostCheck, strings.Join(found, "; "))
	}
	return nil
}

// adminTx runs step on the admin connection in one locked transaction,
// only for the roles that still exist.
func (r *retireRun) adminTx(ctx context.Context, roles []string,
	step func(context.Context, pgx.Tx, []string) error) error {
	tx, err := r.req.Cluster.Admin.Begin(ctx)
	if err != nil {
		return fmt.Errorf("agentguard: beginning retire: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockRoleTx(ctx, tx, r.m.cfg, r.req.PrincipalID); err != nil {
		return err
	}
	have, err := existingRoles(ctx, tx, roles...)
	if err != nil {
		return err
	}
	var present []string
	for _, role := range roles {
		if have[role] {
			present = append(present, role)
		}
	}
	if err := step(ctx, tx, present); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("agentguard: committing retire: %w", err)
	}
	return nil
}

// inherit sets whether pg_sage inherits the roles' privileges. DROP OWNED
// needs it (privileges of the role); it is held only while retiring.
func (r *retireRun) inherit(on bool) func(context.Context, pgx.Tx, []string) error {
	return func(ctx context.Context, tx pgx.Tx, roles []string) error {
		for _, role := range roles {
			stmt := fmt.Sprintf("GRANT %s TO CURRENT_USER WITH INHERIT %v", ident(role), on)
			if _, err := tx.Exec(ctx, "/* pg_sage guard_role v1 */ "+stmt); err != nil {
				return fmt.Errorf("agentguard: %s: %w", stmt, err)
			}
		}
		return nil
	}
}

// restoreMembership drops the temporary INHERIT after a failed retire, so
// pg_sage never keeps an agent role's privileges (AP-14).
func (r *retireRun) restoreMembership(ctx context.Context, roles []string) {
	_ = r.adminTx(context.WithoutCancel(ctx), roles, r.inherit(false))
}

// dropOwned runs DROP OWNED BY for the existing roles in one database:
// it drops what they own and revokes what pg_sage granted them.
func (r *retireRun) dropOwned(ctx context.Context, d ClusterDatabase, roles []string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("agentguard: beginning retire in %s: %w", d.Name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockRoleTx(ctx, tx, r.m.cfg, r.req.PrincipalID); err != nil {
		return err
	}
	have, err := existingRoles(ctx, tx, roles...)
	if err != nil {
		return err
	}
	for _, role := range roles {
		if !have[role] {
			continue
		}
		stmt := "DROP OWNED BY " + ident(role)
		if _, err := tx.Exec(ctx, "/* pg_sage guard_role v1 */ "+stmt); err != nil {
			return fmt.Errorf("agentguard: %s in %s: %w", stmt, d.Name, err)
		}
		r.statements = append(r.statements, "-- in "+d.Name+": "+stmt)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("agentguard: committing retire in %s: %w", d.Name, err)
	}
	return nil
}

// dropRoles drops the roles and verifies they are gone from the cluster.
func (r *retireRun) dropRoles(ctx context.Context, tx pgx.Tx, roles []string) error {
	for _, role := range roles {
		stmt := "DROP ROLE " + ident(role)
		if _, err := tx.Exec(ctx, "/* pg_sage guard_role v1 */ "+stmt); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "2BP01" {
				return fmt.Errorf("%w: %s still depends on objects (%s)", ErrPostCheck,
					role, pg.Detail)
			}
			return fmt.Errorf("agentguard: %s: %w", stmt, err)
		}
		r.statements = append(r.statements, stmt)
	}
	left, err := existingRoles(ctx, tx, roles...)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("%w: roles still present after DROP ROLE", ErrPostCheck)
	}
	return nil
}
