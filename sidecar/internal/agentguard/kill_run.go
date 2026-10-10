package agentguard

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// clusterRun is the kill on one cluster: its monitored databases (the
// first is where roles change and the action is recorded) and their
// configured replicas. It runs as the narrowing contract through the
// first database's executor; when that gate refuses or none is wired, it
// runs directly and writes the local fallback log (§6.2.5).
type clusterRun struct {
	s          *Switch
	k          killScope
	key        string
	targets    []KillTarget
	datnames   []string
	dbs        []DatabaseReport
	inflight   map[string][]int // database_id → backend pids
	registered []ClusterRole    // the scope's principals' roles on this cluster
	replicas   []*replicaRun
	standbys   []ReplicaReport
	statements []string
	prior      map[string]PriorAttrs // principal → prior attributes captured now
	executed   bool
	version    int
}

func newClusterRun(s *Switch, k killScope, key string, targets []KillTarget) *clusterRun {
	r := &clusterRun{s: s, k: k, key: key, targets: targets,
		datnames: make([]string, len(targets)), dbs: make([]DatabaseReport, len(targets)),
		prior: map[string]PriorAttrs{}}
	for i, t := range targets {
		r.dbs[i] = DatabaseReport{Name: t.Name, Replicas: []ReplicaReport{}}
	}
	return r
}

// run contains the cluster through the gate, else directly.
func (r *clusterRun) run(ctx context.Context) {
	ex := r.targets[0].Executor
	var refused error
	if ex != nil {
		req, err := r.policyRequest()
		if err == nil {
			var id int64
			id, err = ex.Apply(ctx, executor.ActionIntent{Request: req, SlotHeld: true,
				Execute: r.execute})
			if r.executed {
				r.dbs[0].ActionID = id
				r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
				return
			}
		}
		refused = err
	}
	for i := range r.dbs {
		r.dbs[i].Direct = true
	}
	r.contain(ctx)
	r.appendFallback(refused)
}

// policyRequest is the narrowing gate request of this run.
func (r *clusterRun) policyRequest() (policy.ActionRequest, error) {
	contract, ok := executor.PolicyContractFor(r.k.actionType)
	if !ok {
		return policy.ActionRequest{}, fmt.Errorf("agentguard: no contract for %s",
			r.k.actionType)
	}
	args, err := json.Marshal(map[string]any{"scope": r.k.scope, "principals": r.k.ids,
		"cluster_key": r.key, "kill_id": r.k.killID})
	if err != nil {
		return policy.ActionRequest{}, fmt.Errorf("agentguard: encoding arguments: %w", err)
	}
	return policy.ActionRequest{Contract: contract, Arguments: args, InternalControl: true,
		Feature: string(policy.ChangeAgentAccess), TargetObjs: []string{"cluster:" + r.key},
		Evidence: map[string]any{"source": "agent_governance", "scope": string(r.k.scope),
			"principals": r.k.ids, "kill_id": r.k.killID, "reason": r.k.reason,
			"actor": r.k.actor}}, nil
}

// execute is Apply's Execute: contain, then record the action.
func (r *clusterRun) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	r.executed = true
	r.contain(ctx)
	return r.record(ctx, decision.DecisionID)
}

// contain runs steps 3-6 on the cluster.
func (r *clusterRun) contain(ctx context.Context) {
	primary := r.targets[0].Pool
	if v, err := serverVersion(ctx, primary); err == nil {
		r.version = v
	}
	for i, t := range r.targets {
		name, err := datnameOf(ctx, t.Pool)
		r.datnames[i] = name
		r.dbs[i].Error = joinErr(r.dbs[i].Error, err)
	}
	if r.k.disablesRoles() {
		r.disableRoles(ctx)
	}
	for i, t := range r.targets {
		n, err := cancelApprovals(ctx, t.Pool, r.k)
		r.dbs[i].ApprovalsCancelled = n
		r.dbs[i].Error = joinErr(r.dbs[i].Error, err)
		c, err := cancelInflight(ctx, t.Pool, r.inflight[t.DatabaseID])
		r.dbs[i].StatementsCancelled = c
		r.dbs[i].Error = joinErr(r.dbs[i].Error, err)
	}
	n, err := terminate(ctx, primary, r.k, r.scopeDatname())
	r.dbs[0].BackendsTerminated = n
	r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
	r.containReplicas(ctx)
}

// scopeDatname limits a database kill to its database; "" is the cluster.
func (r *clusterRun) scopeDatname() string {
	if r.k.scope == KillScopeDatabase {
		return r.datnames[0]
	}
	return ""
}

// disableRoles is step 5: prior attributes, then NOLOGIN CONNECTION
// LIMIT 0 per role, then the killed status with prior_attrs in the
// control database.
func (r *clusterRun) disableRoles(ctx context.Context) {
	primary := r.targets[0].Pool
	rows, err := readRoleAttrs(ctx, primary, r.k.roles(), r.k.all)
	if err != nil {
		r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
		return
	}
	owner := r.roleOwners()
	for _, row := range rows {
		if pid, ok := owner[row.name]; ok && !row.attrs.killed() {
			if r.prior[pid] == nil {
				r.prior[pid] = PriorAttrs{}
			}
			r.prior[pid][row.name] = row.attrs
		}
		stmt, err := r.s.disableRole(ctx, primary, row.name)
		r.statements = append(r.statements, stmt)
		if err != nil {
			r.dbs[0].Error = joinErr(r.dbs[0].Error, fmt.Errorf("agentguard: %w", err))
			continue
		}
		r.dbs[0].RolesDisabled++
	}
	r.storeKilled(ctx)
}

// roleOwners maps role names to principals: the scope's own ids, and the
// registered roles of this cluster.
func (r *clusterRun) roleOwners() map[string]string {
	out := map[string]string{}
	for _, id := range r.k.ids {
		out[BrokerRoleName(id)], out[LoginRoleName(id)] = id, id
	}
	for _, cr := range r.registered {
		out[cr.BrokerRole], out[cr.LoginRole] = cr.PrincipalID, cr.PrincipalID
	}
	return out
}

// storeKilled marks each registered principal's roles killed. Prior
// attributes are stored only from an active registration, so a second
// kill never overwrites what the first recorded.
func (r *clusterRun) storeKilled(ctx context.Context) {
	for _, cr := range r.registered {
		var prior json.RawMessage
		if cr.Status == RoleStatusActive && len(r.prior[cr.PrincipalID]) > 0 {
			raw, err := json.Marshal(r.prior[cr.PrincipalID])
			if err != nil {
				r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
				continue
			}
			prior = raw
		}
		err := r.s.store.SetClusterRoleStatus(ctx, cr.PrincipalID, r.key, RoleStatusKilled,
			prior)
		r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
	}
}

// record writes the action_log row of a gated run.
func (r *clusterRun) record(ctx context.Context, decisionID int64) (int64, error) {
	before, err := json.Marshal(r.prior)
	if err != nil {
		return 0, fmt.Errorf("agentguard: encoding prior attributes: %w", err)
	}
	after, err := json.Marshal(r.dbs)
	if err != nil {
		return 0, fmt.Errorf("agentguard: encoding the kill result: %w", err)
	}
	var principal *string
	if len(r.k.ids) == 1 && !r.k.all {
		principal = &r.k.ids[0]
	}
	var id int64
	err = r.targets[0].Pool.QueryRow(ctx, `/* pg_sage guard_kill_record v1 */
		INSERT INTO sage.action_log (action_type, sql_executed, before_state, after_state,
			outcome, decision_id, principal_id, measured_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6::bigint, 0), $7, now())
		RETURNING id`, r.k.actionType, trimStatements(r.statements), before, after,
		r.outcome(), decisionID, principal).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agentguard: %s ran but recording it failed: %w",
			r.k.actionType, err)
	}
	return id, nil
}

func (r *clusterRun) outcome() string {
	for _, d := range r.dbs {
		if d.Error != "" {
			return "failed"
		}
	}
	return "success"
}

// appendFallback audits a direct run locally.
func (r *clusterRun) appendFallback(refused error) {
	e := FallbackEntry{ActionType: r.k.actionType, Database: r.targets[0].Name,
		Scope: string(r.k.scope), Target: r.k.database, Reason: r.k.reason,
		Actor: r.k.actor, Statements: r.statements, Outcome: r.outcome()}
	if len(r.k.ids) == 1 && !r.k.all {
		e.PrincipalID = r.k.ids[0]
	}
	if refused != nil {
		e.Error = "gate: " + refused.Error()
	}
	if err := r.s.fallback.Append(e); err != nil {
		r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
	}
}
