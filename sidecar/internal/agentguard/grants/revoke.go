package grants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
)

// errSchemaInUse is a schema USAGE row whose schema still has an active
// relation grant: it goes with the schema's last grant.
var errSchemaInUse = fmt.Errorf("%w: schema USAGE goes with the schema's last grant",
	agentguard.ErrInvalid)

func (r RevokeRequest) validate() error {
	switch {
	case r.GrantID <= 0:
		return invalidf("grant id must be positive")
	case r.Cause != CauseExpired && r.Cause != CauseOperator:
		return invalidf("cause must be expired or operator, got %q", r.Cause)
	case r.Target.Pool == nil || r.Target.Executor == nil:
		return invalidf("database %q has no pg_sage connection or executor", r.Target.Name)
	case r.Fence.Holder != "" && r.Fence.Control == nil:
		return invalidf("a fenced revoke needs the control database")
	}
	return nil
}

// Revoke runs guard_revoke on one registry row: REVOKE … GRANTED BY the
// recorded grantor of the columns no other active grant of the principal
// still covers, the schema USAGE with the schema's last grant, then a
// residue check. Another grantor's residue marks the row
// revoke_incomplete. guard_revoke is narrowing: the gate passes it during
// an emergency stop and at every trust level.
func (m *Manager) Revoke(ctx context.Context, req RevokeRequest) (RevokeResult, error) {
	if err := req.validate(); err != nil {
		return RevokeResult{}, err
	}
	g, err := Get(ctx, req.Target.Pool, req.GrantID)
	if err != nil {
		return RevokeResult{}, err
	}
	if g.RevokedAt != nil {
		return RevokeResult{}, fmt.Errorf("%w: grant %d is %s", ErrNotActive, g.ID, g.State)
	}
	gateReq, err := gateRequest(executor.ActionTypeGuardRevoke, g.PrincipalID, req.Target,
		[]string{g.ObjectKind + ":" + g.ObjectName}, map[string]any{"grant_id": g.ID,
			"cause": req.Cause, "approved_by": req.ApprovedBy}, req.ApprovedBy > 0)
	if err != nil {
		return RevokeResult{}, err
	}
	run := &revokeRun{req: req, broker: agentguard.BrokerRoleName(g.PrincipalID)}
	actionID, err := req.Target.Executor.Apply(ctx, executor.ActionIntent{Request: gateReq,
		Authorize: authorizer(req.Target.Executor, gateReq), SlotHeld: true,
		Execute: run.execute})
	if err != nil {
		return RevokeResult{}, err
	}
	run.result.ActionID = actionID
	return run.result, nil
}

// revokeRun is one guard_revoke moving through Apply.
type revokeRun struct {
	req        RevokeRequest
	broker     string
	statements []string
	schemaRows []int64
	result     RevokeResult
}

func (r *revokeRun) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	release, err := holdFence(ctx, r.req.Fence)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := r.req.Target.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("grants: beginning revoke transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	g, err := r.lockRow(ctx, tx)
	if err != nil {
		return 0, err
	}
	detail, err := r.revokeGrant(ctx, tx, g)
	if err != nil {
		return 0, err
	}
	actionID, err := recordAction(ctx, tx, actionRecord{actionType: executor.ActionTypeGuardRevoke,
		principalID: g.PrincipalID, approvedBy: r.req.ApprovedBy, decisionID: decision.DecisionID,
		statements: r.statements, before: map[string]any{"grant_id": g.ID,
			"object": g.ObjectName, "columns": g.Columns, "grantor": g.Grantor},
		after: map[string]any{"cause": r.req.Cause, "residue": r.result.Residue}})
	if err != nil {
		return 0, err
	}
	if err := r.markRevoked(ctx, tx, g, detail, actionID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("grants: committing revoke of %d: %w", g.ID, err)
	}
	return actionID, nil
}

// lockRow takes the principal's grant lock and the row; a row revoked by a
// concurrent pass is ErrNotActive.
func (r *revokeRun) lockRow(ctx context.Context, tx pgx.Tx) (Grant, error) {
	g, err := Get(ctx, tx, r.req.GrantID)
	if err != nil {
		return g, err
	}
	if err := lockGrants(ctx, tx, g.PrincipalID); err != nil {
		return g, err
	}
	g, err = scanGrant(tx.QueryRow(ctx, `/* pg_sage guard_revoke v1 */ SELECT `+
		grantColumns+` FROM sage.guard_grants WHERE id = $1 FOR UPDATE`, r.req.GrantID))
	if err != nil {
		return g, fmt.Errorf("grants: locking grant %d: %w", r.req.GrantID, err)
	}
	if g.RevokedAt != nil {
		return g, fmt.Errorf("%w: grant %d is %s", ErrNotActive, g.ID, g.State)
	}
	if g.ObjectKind == KindSchema {
		n, err := activeInSchema(ctx, tx, g.PrincipalID, g.SchemaOID, 0)
		if err != nil {
			return g, err
		}
		if n > 0 {
			return g, errSchemaInUse
		}
	}
	return g, nil
}

func (r *revokeRun) exec(ctx context.Context, tx pgx.Tx, stmt string) error {
	if _, err := tx.Exec(ctx, "/* pg_sage guard_revoke v1 */ "+stmt); err != nil {
		return fmt.Errorf("grants: %s: %w", stmt, err)
	}
	r.statements = append(r.statements, stmt)
	return nil
}

// revokeGrant revokes a relation row (and its schema's USAGE with the last
// one) or a schema row, and returns the row's revoke detail.
func (r *revokeRun) revokeGrant(ctx context.Context, tx pgx.Tx, g Grant) (string, error) {
	var roleExists bool
	if err := tx.QueryRow(ctx, `/* pg_sage guard_revoke v1 */ SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`,
		r.broker).Scan(&roleExists); err != nil {
		return "", fmt.Errorf("grants: reading role %s: %w", r.broker, err)
	}
	if !roleExists {
		return "the role was dropped", nil // its privileges went with it
	}
	if g.ObjectKind == KindSchema {
		return r.revokeSchema(ctx, tx, g)
	}
	name, err := relationName(ctx, tx, g.ObjectOID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "the object was dropped", nil
	}
	if err != nil {
		return "", err
	}
	cols, err := r.uncovered(ctx, tx, g)
	if err != nil {
		return "", err
	}
	if len(cols) > 0 {
		stmt := fmt.Sprintf("REVOKE SELECT (%s) ON TABLE %s FROM %s GRANTED BY %s",
			identList(cols), name, ident(r.broker), ident(g.Grantor))
		if err := r.exec(ctx, tx, stmt); err != nil {
			return "", err
		}
		if err := ownResidue(ctx, tx, g, cols, r.broker); err != nil {
			return "", err
		}
	}
	detail, err := r.foreignResidue(ctx, tx, g)
	if err != nil {
		return "", err
	}
	return detail, r.revokeSchemaWithLast(ctx, tx, g)
}

func relationName(ctx context.Context, tx pgx.Tx, oid uint32) (string, error) {
	var schema, rel string
	err := tx.QueryRow(ctx, `/* pg_sage guard_revoke v1 */
		SELECT n.nspname::text, c.relname::text FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = $1::int8::oid`, int64(oid)).Scan(&schema, &rel)
	if err != nil {
		return "", fmt.Errorf("grants: resolving relation %d: %w", oid, err)
	}
	return ident(schema, rel), nil
}

// uncovered is the row's columns that still exist and that no other active
// grant of the principal on the object covers: PostgreSQL keeps one ACL
// entry per column, so revoking a column another grant lists would end
// that grant too.
func (r *revokeRun) uncovered(ctx context.Context, tx pgx.Tx, g Grant) ([]string, error) {
	rows, err := tx.Query(ctx, `/* pg_sage guard_revoke v1 */
		SELECT a.attname::text FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = $1::int8::oid AND a.attnum > 0 AND NOT a.attisdropped
		  AND a.attname = ANY($2)
		  AND NOT EXISTS (SELECT 1 FROM sage.guard_grants o
		    WHERE o.principal_id = $3 AND o.object_oid = $1::int8::oid AND o.id <> $4
		      AND o.state = 'active' AND o.revoked_at IS NULL AND o.object_kind = 'relation'
		      AND a.attname = ANY(o.columns))
		ORDER BY a.attnum`, int64(g.ObjectOID), g.Columns, g.PrincipalID, g.ID)
	if err != nil {
		return nil, fmt.Errorf("grants: columns to revoke of %d: %w", g.ID, err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ownResidue fails the revoke when the recorded grantor's privilege is
// still on a revoked column (the post-check: no residue for (grantee,
// grantor)).
func ownResidue(ctx context.Context, tx pgx.Tx, g Grant, cols []string, broker string) error {
	var n int
	err := tx.QueryRow(ctx, `/* pg_sage guard_revoke v1 */
		SELECT count(*) FROM pg_catalog.pg_attribute a, pg_catalog.aclexplode(a.attacl) x
		WHERE a.attrelid = $1::int8::oid AND a.attname = ANY($2)
		  AND x.grantee = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $3)
		  AND x.grantor = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $4)`,
		int64(g.ObjectOID), cols, broker, g.Grantor).Scan(&n)
	if err != nil {
		return fmt.Errorf("grants: residue check of %d: %w", g.ID, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %d privileges of %s from %s remain on %s",
			agentguard.ErrPostCheck, n, broker, g.Grantor, g.ObjectName)
	}
	return nil
}

// foreignResidue lists the broker's privileges on the object from other
// grantors (agentguard.ForeignGrants); pg_sage cannot revoke them.
func (r *revokeRun) foreignResidue(ctx context.Context, tx pgx.Tx, g Grant) (string, error) {
	residue, err := objectResidue(ctx, tx, r.broker, g.ObjectOID)
	if err != nil || len(residue) == 0 {
		return "", err
	}
	r.result.Residue = residue
	var parts []string
	for _, x := range residue {
		parts = append(parts, "other grantor "+x.Grantor+": "+x.Object)
	}
	return truncate(strings.Join(parts, "; "), 4000), nil
}

// objectResidue is ForeignGrants narrowed to one relation and its columns.
func objectResidue(ctx context.Context, q Querier, broker string,
	oid uint32) ([]agentguard.Residue, error) {
	var desc string
	err := q.QueryRow(ctx, `/* pg_sage guard_revoke v1 */
		SELECT COALESCE(pg_catalog.pg_describe_object(
		  'pg_catalog.pg_class'::pg_catalog.regclass, $1::int8::oid, 0), '')`,
		int64(oid)).Scan(&desc)
	if err != nil {
		return nil, fmt.Errorf("grants: describing relation %d: %w", oid, err)
	}
	if desc == "" {
		return nil, nil // dropped: its privileges went with it
	}
	all, err := agentguard.ForeignGrants(ctx, q, []string{broker})
	if err != nil {
		return nil, err
	}
	var out []agentguard.Residue
	for _, x := range all {
		if x.Object == desc || strings.HasSuffix(x.Object, " of "+desc) {
			out = append(out, x)
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
