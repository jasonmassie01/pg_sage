package grants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Grant runs guard_grant through the target's executor under the
// operator's approval (L2): it checks the principal (D1-D4), resolves
// every object and its column list (classification, P1, pg_sage's own
// grant options), then grants, post-checks with aclexplode and records the
// registry rows and the action in one transaction.
func (m *Manager) Grant(ctx context.Context, req GrantRequest) (GrantResult, error) {
	if err := req.validate(m.cfg.MaxDuration); err != nil {
		return GrantResult{}, err
	}
	p, err := m.checkPrincipal(ctx, req)
	if err != nil {
		return GrantResult{}, err
	}
	rels, err := m.planRelations(ctx, req)
	if err != nil {
		return GrantResult{}, err
	}
	gateReq, err := gateRequest(executor.ActionTypeGuardGrant, p.ID, req.Target,
		targetsOf(rels), map[string]any{"approved_by": req.Approval.ApprovedBy,
			"approval_id": req.Approval.ApprovalID}, true)
	if err != nil {
		return GrantResult{}, err
	}
	run := &grantRun{req: req, broker: p.BrokerRole(), rels: rels}
	actionID, err := req.Target.Executor.Apply(ctx, executor.ActionIntent{Request: gateReq,
		Authorize: authorizer(req.Target.Executor, gateReq), SlotHeld: true,
		Execute: run.execute})
	if err != nil {
		return GrantResult{}, err
	}
	res := GrantResult{ActionID: actionID, Grants: run.grants}
	for _, r := range rels {
		res.Excluded = append(res.Excluded, r.excluded...)
	}
	return res, nil
}

func targetsOf(rels []*relation) []string {
	var out []string
	for _, r := range rels {
		out = append(out, "table:"+r.qualified())
	}
	return out
}

// gateRequest is a typed internal request of a grant contract, change
// class agent_access, attributed to the principal.
func gateRequest(actionType, principalID string, t Target, objects []string,
	evidence map[string]any, operator bool) (policy.ActionRequest, error) {
	contract, ok := executor.PolicyContractFor(actionType)
	if !ok {
		return policy.ActionRequest{}, fmt.Errorf("grants: no contract for %s", actionType)
	}
	args, err := json.Marshal(map[string]any{"principal_id": principalID,
		"database_id": t.ID, "objects": objects})
	if err != nil {
		return policy.ActionRequest{}, fmt.Errorf("grants: encoding arguments: %w", err)
	}
	ev := map[string]any{"source": "agent_governance", "principal_id": principalID,
		"database_id": t.ID}
	for k, v := range evidence {
		ev[k] = v
	}
	return policy.ActionRequest{Contract: contract, Arguments: args, InternalControl: true,
		Feature: string(policy.ChangeAgentAccess), OperatorApproved: operator,
		TargetObjs: objects, Evidence: ev}, nil
}

// authorizer asks the executor's standing gate at both of Apply's
// authorization points.
func authorizer(ex agentguard.Applier, req policy.ActionRequest) func(context.Context) (
	executor.ActionPolicyDecision, error) {
	calls := 0
	return func(ctx context.Context) (executor.ActionPolicyDecision, error) {
		calls++
		return executor.AuthorizeTyped(ctx, ex.StandingPolicyGate(), req, calls > 1)
	}
}

// grantRun is one guard_grant moving through Apply.
type grantRun struct {
	req        GrantRequest
	broker     string
	rels       []*relation
	statements []string
	grants     []Grant
}

func (r *grantRun) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	tx, err := r.req.Target.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("grants: beginning grant transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockPrincipal(ctx, tx, r.req.PrincipalID, r.broker); err != nil {
		return 0, err
	}
	for _, rel := range r.rels {
		if err := r.grantRelation(ctx, tx, rel); err != nil {
			return 0, err
		}
	}
	actionID, err := recordAction(ctx, tx, actionRecord{actionType: executor.ActionTypeGuardGrant,
		principalID: r.req.PrincipalID, approvedBy: r.req.Approval.ApprovedBy,
		approvalID: r.req.Approval.ApprovalID, decisionID: decision.DecisionID,
		statements: r.statements, before: map[string]any{"database_id": r.req.Target.ID},
		after: map[string]any{"objects": targetsOf(r.rels), "reason": r.req.Reason,
			"duration_seconds": int64(r.req.Duration / time.Second)}})
	if err != nil {
		return 0, err
	}
	if err := r.record(ctx, tx, actionID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("grants: committing grant: %w", err)
	}
	return actionID, nil
}

// lockPrincipal takes the principal's grant lock and checks its broker
// role exists on this cluster.
func lockPrincipal(ctx context.Context, tx pgx.Tx, principalID, broker string) error {
	if err := lockGrants(ctx, tx, principalID); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `/* pg_sage guard_grant v1 */ SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`, broker).Scan(&exists); err != nil {
		return fmt.Errorf("grants: reading role %s: %w", broker, err)
	}
	if !exists {
		return deny(ReasonNoRoles, "", "role %s does not exist on this cluster; approve "+
			"guard_role_ensure for it first", broker)
	}
	return nil
}

func (r *grantRun) exec(ctx context.Context, tx pgx.Tx, stmt string) error {
	if _, err := tx.Exec(ctx, "/* pg_sage guard_grant v1 */ "+stmt); err != nil {
		return fmt.Errorf("grants: %s: %w", stmt, err)
	}
	r.statements = append(r.statements, stmt)
	return nil
}

// grantRelation grants the column list and post-checks it: every column
// shows the broker role as grantee of SELECT from pg_sage's own role.
func (r *grantRun) grantRelation(ctx context.Context, tx pgx.Tx, rel *relation) error {
	stmt := fmt.Sprintf("GRANT SELECT (%s) ON TABLE %s TO %s", identList(rel.grant),
		ident(rel.schema, rel.name), ident(r.broker))
	if err := r.exec(ctx, tx, stmt); err != nil {
		return err
	}
	var n int
	err := tx.QueryRow(ctx, `/* pg_sage guard_grant v1 */
		SELECT count(DISTINCT a.attname) FROM pg_catalog.pg_attribute a,
		  pg_catalog.aclexplode(a.attacl) x
		WHERE a.attrelid = $1::int8::oid AND a.attname = ANY($2)
		  AND x.grantee = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $3)
		  AND x.grantor = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
		  AND x.privilege_type = 'SELECT'`, int64(rel.oid), rel.grant, r.broker).Scan(&n)
	if err != nil {
		return fmt.Errorf("grants: post-check of %s: %w", rel.qualified(), err)
	}
	if n != len(rel.grant) {
		return fmt.Errorf("%w: %s shows %d of %d granted columns from %s",
			agentguard.ErrPostCheck, rel.qualified(), n, len(rel.grant), rel.me)
	}
	return nil
}

// record writes each relation's registry row and the schema USAGE rows:
// granted with the first relation grant in a schema, extended after.
func (r *grantRun) record(ctx context.Context, tx pgx.Tx, actionID int64) error {
	secs := r.req.Duration.Seconds()
	for _, rel := range r.rels {
		g, err := scanGrant(tx.QueryRow(ctx, insertGrantSQL, r.req.Target.ID,
			r.req.PrincipalID, CapabilityRead, KindRelation, int64(rel.oid), rel.qualified(),
			int64(rel.schemaOID), rel.grant, []string{"SELECT"}, rel.me, secs, actionID))
		if err != nil {
			return fmt.Errorf("grants: recording grant on %s: %w", rel.qualified(), err)
		}
		r.grants = append(r.grants, g)
		if err := r.schemaRow(ctx, tx, rel, g, actionID); err != nil {
			return err
		}
	}
	return nil
}

func (r *grantRun) schemaRow(ctx context.Context, tx pgx.Tx, rel *relation, g Grant,
	actionID int64) error {
	open, err := scanGrant(tx.QueryRow(ctx, openSchemaRowSQL, r.req.PrincipalID,
		int64(rel.schemaOID)))
	if err == nil {
		_, err = scanGrant(tx.QueryRow(ctx, extendSchemaRowSQL, open.ID, g.ExpiresAt))
		if err != nil {
			return fmt.Errorf("grants: extending schema USAGE of %s: %w", rel.schema, err)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("grants: reading schema USAGE of %s: %w", rel.schema, err)
	}
	stmt := fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", ident(rel.schema), ident(r.broker))
	if err := r.exec(ctx, tx, stmt); err != nil {
		return err
	}
	s, err := scanGrant(tx.QueryRow(ctx, insertGrantSQL, r.req.Target.ID, r.req.PrincipalID,
		CapabilityRead, KindSchema, int64(rel.schemaOID), rel.schema, int64(rel.schemaOID),
		[]string{}, []string{"USAGE"}, rel.me, r.req.Duration.Seconds(), actionID))
	if err != nil {
		return fmt.Errorf("grants: recording schema USAGE of %s: %w", rel.schema, err)
	}
	r.grants = append(r.grants, s)
	return nil
}

// actionRecord is one action_log row of a grant contract.
type actionRecord struct {
	actionType  string
	principalID string
	approvedBy  int
	approvalID  int64
	decisionID  int64
	statements  []string
	before      map[string]any
	after       map[string]any
}

// recordAction writes the action_log row in the contract's transaction,
// with the principal's provenance.
func recordAction(ctx context.Context, tx pgx.Tx, a actionRecord) (int64, error) {
	before, err := json.Marshal(a.before)
	if err != nil {
		return 0, fmt.Errorf("grants: encoding action state: %w", err)
	}
	after, err := json.Marshal(a.after)
	if err != nil {
		return 0, fmt.Errorf("grants: encoding action state: %w", err)
	}
	var id int64
	err = tx.QueryRow(ctx, `/* pg_sage guard_grant_record v1 */
		INSERT INTO sage.action_log (action_type, sql_executed, before_state, after_state,
			outcome, approved_by, approved_at, decision_id, principal_id, approval_id,
			measured_at)
		VALUES ($1, $2, $3, $4, 'success', NULLIF($5::int, 0),
			CASE WHEN $5::int > 0 THEN now() END, NULLIF($6::bigint, 0), $7,
			NULLIF($8::bigint, 0), now())
		RETURNING id`, a.actionType, strings.Join(a.statements, ";\n"), before, after,
		a.approvedBy, a.decisionID, a.principalID, a.approvalID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("grants: recording %s for %s: %w", a.actionType,
			a.principalID, err)
	}
	return id, nil
}
