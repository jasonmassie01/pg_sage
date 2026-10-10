package agentguard

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// guard_unfreeze on one cluster: restore each role's prior attributes and
// set a new broker password (old passwords fail), in one transaction on
// pg_sage's pool, then store the new sealed credential.

type unfreezeRun struct {
	role   ClusterRole
	target KillTarget
}

// unfreezePlan maps each of the principal's clusters to the configured
// database whose executor runs its unfreeze. A cluster with none fails
// the whole unfreeze before anything changes: its roles stay disabled.
func (s *Switch) unfreezePlan(ctx context.Context, principalID string) ([]unfreezeRun,
	error) {
	if s.keyring == nil {
		return nil, ErrEncryptionKeyRequired
	}
	roles, err := s.store.ClusterRoles(ctx, principalID)
	if err != nil {
		return nil, err
	}
	targets, err := s.targets(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentguard: listing databases to unfreeze on: %w", err)
	}
	byKey := map[string]KillTarget{}
	for _, group := range groupByCluster(targets) {
		for _, t := range group {
			if t.Executor != nil && t.ClusterKey != "" {
				byKey[t.ClusterKey] = t
				break
			}
		}
	}
	var plan []unfreezeRun
	for _, cr := range roles {
		if cr.Status == RoleStatusRetired {
			continue
		}
		t, ok := byKey[cr.ClusterKey]
		if !ok {
			return nil, fmt.Errorf("%w: cluster %s of %s has no configured database with an "+
				"executor; its roles stay disabled", ErrUnavailable, cr.ClusterKey, principalID)
		}
		plan = append(plan, unfreezeRun{role: cr, target: t})
	}
	return plan, nil
}

// restoreAttrs is what the roles get back: prior_attrs, else the spec's
// attributes (broker LOGIN, direct lane NOLOGIN in G1).
func (u unfreezeRun) restoreAttrs(rc RoleConfig) (PriorAttrs, error) {
	prior, err := ParsePriorAttrs(u.role.PriorAttrs)
	if err != nil {
		return nil, err
	}
	out := PriorAttrs{
		u.role.BrokerRole: {Login: true, ConnectionLimit: rc.BrokerConnectionLimit},
		u.role.LoginRole:  {Login: false, ConnectionLimit: rc.ConnectionLimit},
	}
	for role := range out {
		if a, ok := prior[role]; ok {
			out[role] = a
		}
	}
	return out, nil
}

// apply runs guard_unfreeze through the cluster's executor, operator
// approved by the admin who completed the sign-off.
func (u unfreezeRun) apply(ctx context.Context, s *Switch, req UnfreezeRequest,
	requestID int64) (UnfreezeCluster, error) {
	attrs, err := u.restoreAttrs(s.cfg.Roles)
	if err != nil {
		return UnfreezeCluster{}, err
	}
	contract, ok := executor.PolicyContractFor(executor.ActionTypeGuardUnfreeze)
	if !ok {
		return UnfreezeCluster{}, fmt.Errorf("agentguard: no guard_unfreeze contract")
	}
	args, err := json.Marshal(map[string]any{"principal_id": u.role.PrincipalID,
		"cluster_key": u.role.ClusterKey})
	if err != nil {
		return UnfreezeCluster{}, fmt.Errorf("agentguard: encoding arguments: %w", err)
	}
	gateReq := policy.ActionRequest{Contract: contract, Arguments: args,
		InternalControl: true, OperatorApproved: true,
		Feature:    string(policy.ChangeAgentAccess),
		TargetObjs: []string{"role:" + u.role.LoginRole, "role:" + u.role.BrokerRole},
		Evidence: map[string]any{"source": "agent_governance",
			"principal_id": u.role.PrincipalID, "approved_by": req.ActorUserID,
			"request_id": requestID, "reason": req.Reason}}
	run := &unfreezeExec{u: u, s: s, req: req, requestID: requestID, attrs: attrs}
	id, err := u.target.Executor.Apply(ctx, executor.ActionIntent{Request: gateReq,
		SlotHeld: true, Execute: run.execute})
	if err != nil {
		return UnfreezeCluster{}, err
	}
	return UnfreezeCluster{ClusterKey: u.role.ClusterKey, ActionID: id, Rotated: true,
		Restored: attrs}, nil
}

type unfreezeExec struct {
	u          unfreezeRun
	s          *Switch
	req        UnfreezeRequest
	requestID  int64
	attrs      PriorAttrs
	statements []string
}

func (e *unfreezeExec) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	secret, err := newBrokerSecret()
	if err != nil {
		return 0, err
	}
	if err := e.alterRoles(ctx, secret); err != nil {
		return 0, err
	}
	cr := e.u.role
	cr.Status = RoleStatusActive
	if err := e.s.store.saveClusterRole(ctx, e.s.keyring, cr, secret); err != nil {
		return 0, fmt.Errorf("the roles of %s were restored but their new credential was "+
			"not stored; the broker login fails until the next ensure: %w", cr.PrincipalID,
			err)
	}
	return e.record(ctx, decision.DecisionID)
}

// alterRoles restores both roles and sets the new password in one
// transaction, post-checked before it commits.
func (e *unfreezeExec) alterRoles(ctx context.Context, secret string) error {
	verifier, err := newScramVerifier(secret)
	if err != nil {
		return err
	}
	cr := e.u.role
	return pgx.BeginFunc(ctx, e.u.target.Pool, func(tx pgx.Tx) error {
		if err := lockRoleTx(ctx, tx, e.s.cfg.Roles, cr.PrincipalID); err != nil {
			return err
		}
		for _, role := range []string{cr.BrokerRole, cr.LoginRole} {
			a := e.attrs[role]
			stmt := "ALTER ROLE " + ident(role) + " WITH " +
				alterAttributes(a.Login, a.ConnectionLimit)
			secretPart := ""
			if role == cr.BrokerRole {
				secretPart = " PASSWORD " + literal(verifier)
			}
			if _, err := tx.Exec(ctx, "/* pg_sage guard_unfreeze v1 */ "+stmt+
				secretPart); err != nil {
				return fmt.Errorf("agentguard: %s: %w", stmt, err)
			}
			if secretPart != "" {
				stmt += " PASSWORD '<scram verifier>'"
			}
			e.statements = append(e.statements, stmt)
		}
		return e.postCheck(ctx, tx)
	})
}

// postCheck reads both roles back: they must have exactly the restored
// attributes.
func (e *unfreezeExec) postCheck(ctx context.Context, tx pgx.Tx) error {
	cr := e.u.role
	rows, err := readRoleAttrs(ctx, tx, []string{cr.BrokerRole, cr.LoginRole}, false)
	if err != nil {
		return err
	}
	if len(rows) != 2 {
		return fmt.Errorf("%w: expected both roles of %s, found %d", ErrPostCheck,
			cr.PrincipalID, len(rows))
	}
	for _, row := range rows {
		if row.attrs != e.attrs[row.name] {
			return fmt.Errorf("%w: %s has %+v, want %+v", ErrPostCheck, row.name, row.attrs,
				e.attrs[row.name])
		}
	}
	return nil
}

func (e *unfreezeExec) record(ctx context.Context, decisionID int64) (int64, error) {
	before, err := json.Marshal(map[string]any{"principal_id": e.u.role.PrincipalID,
		"cluster_key": e.u.role.ClusterKey, "status": e.u.role.Status})
	if err != nil {
		return 0, fmt.Errorf("agentguard: encoding action state: %w", err)
	}
	after, err := json.Marshal(map[string]any{"restored": e.attrs, "rotated": true,
		"reason": e.req.Reason})
	if err != nil {
		return 0, fmt.Errorf("agentguard: encoding action state: %w", err)
	}
	var id int64
	err = e.u.target.Pool.QueryRow(ctx, `/* pg_sage guard_unfreeze_record v1 */
		INSERT INTO sage.action_log (action_type, sql_executed, before_state, after_state,
			outcome, approved_by, approved_at, decision_id, principal_id, approval_id,
			measured_at)
		VALUES ('guard_unfreeze', $1, $2, $3, 'success', $4, now(), NULLIF($5::bigint, 0),
			$6, NULLIF($7::bigint, 0), now())
		RETURNING id`, trimStatements(e.statements), before, after, e.req.ActorUserID,
		decisionID, e.u.role.PrincipalID, e.requestID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agentguard: guard_unfreeze for %s ran but recording it "+
			"failed: %w", e.u.role.PrincipalID, err)
	}
	return id, nil
}
