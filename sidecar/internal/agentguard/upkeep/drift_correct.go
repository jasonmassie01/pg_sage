package upkeep

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// correct runs stmts, the narrowing corrections governance owns for role
// in db, as one guard_revoke through db's executor (narrowing: it runs
// under an emergency stop and at every trust level), and records them on
// d. Without an executor, or when the gate refuses, they become d's Fix.
func (p *clusterPass) correct(ctx context.Context, db agentguard.KillTarget,
	cr agentguard.ClusterRole, role string, d *RoleDrift, stmts []string) error {
	if len(stmts) == 0 {
		return nil
	}
	if db.Executor == nil {
		d.Fix = joinFix(d.Fix, "-- pg_sage has no executor on "+db.Name+": "+
			strings.Join(stmts, "; ")+";")
		return nil
	}
	if err := p.f.check(ctx, p.r.control); err != nil {
		return err
	}
	id, err := applyCorrection(ctx, db, cr, role, stmts)
	if err != nil {
		d.Fix = joinFix(d.Fix, "-- correction refused ("+err.Error()+"): "+
			strings.Join(stmts, "; ")+";")
		return nil
	}
	d.Corrected = append(d.Corrected, stmts...)
	d.ActionIDs = append(d.ActionIDs, id)
	return nil
}

func joinFix(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

func applyCorrection(ctx context.Context, db agentguard.KillTarget,
	cr agentguard.ClusterRole, role string, stmts []string) (int64, error) {
	contract, ok := executor.PolicyContractFor(executor.ActionTypeGuardRevoke)
	if !ok {
		return 0, fmt.Errorf("upkeep: no guard_revoke contract")
	}
	args, err := json.Marshal(map[string]any{"principal_id": cr.PrincipalID, "role": role,
		"cluster_key": cr.ClusterKey})
	if err != nil {
		return 0, fmt.Errorf("upkeep: encoding arguments: %w", err)
	}
	req := policy.ActionRequest{Contract: contract, Arguments: args, InternalControl: true,
		Feature: string(policy.ChangeAgentAccess), TargetObjs: []string{"role:" + role},
		Evidence: map[string]any{"source": "agent_governance", "scheduled": JobDriftCorrection,
			"principal_id": cr.PrincipalID, "role": role, "statements": stmts}}
	before, err := json.Marshal(map[string]any{"principal_id": cr.PrincipalID,
		"role": role, "cluster_key": cr.ClusterKey})
	if err != nil {
		return 0, fmt.Errorf("upkeep: encoding action state: %w", err)
	}
	after, err := json.Marshal(map[string]any{"scheduled": JobDriftCorrection,
		"cause": "drift", "statements": stmts})
	if err != nil {
		return 0, fmt.Errorf("upkeep: encoding action state: %w", err)
	}
	run := correction{db: db, cr: cr, stmts: stmts, before: before, after: after}
	calls := 0
	return db.Executor.Apply(ctx, executor.ActionIntent{Request: req,
		Authorize: func(ctx context.Context) (executor.ActionPolicyDecision, error) {
			calls++
			return executor.AuthorizeTyped(ctx, db.Executor.StandingPolicyGate(), req,
				calls > 1)
		}, SlotHeld: true, Execute: run.execute})
}

type correction struct {
	db            agentguard.KillTarget
	cr            agentguard.ClusterRole
	stmts         []string
	before, after []byte
}

// execute runs the statements and records them in one transaction.
func (c correction) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, c.db.Pool, func(tx pgx.Tx) error {
		for _, s := range c.stmts {
			if _, err := tx.Exec(ctx, "/* pg_sage agent_drift v1 */ "+s); err != nil {
				return fmt.Errorf("upkeep: %s: %w", s, err)
			}
		}
		return tx.QueryRow(ctx, `/* pg_sage agent_drift_record v1 */
			INSERT INTO sage.action_log (action_type, sql_executed, before_state,
				after_state, outcome, decision_id, principal_id, measured_at)
			VALUES ($1, $2, $3, $4, 'success', NULLIF($5::bigint, 0), $6, now())
			RETURNING id`, executor.ActionTypeGuardRevoke, strings.Join(c.stmts, ";\n"),
			c.before, c.after, decision.DecisionID, c.cr.PrincipalID).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}
