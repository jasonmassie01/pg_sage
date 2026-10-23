package upkeep

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/agentguard"
)

const dueRotateSQL = `/* pg_sage agent_upkeep_rotate_due v1 */
SELECT r.principal_id, r.cluster_key, r.rotated_at
FROM sage.guard_cluster_roles r
JOIN sage.guard_principals p ON p.id = r.principal_id
WHERE r.status = 'active' AND p.status = 'active'
  AND r.rotated_at <= now() - $1::interval
  AND (r.rotated_at, r.principal_id, r.cluster_key) > ($2::timestamptz, $3::text, $4::text)
ORDER BY r.rotated_at, r.principal_id, r.cluster_key
LIMIT $5`

func (r *Runner) dueRotations(ctx context.Context, after due) ([]due, error) {
	rows, err := r.control.Query(ctx, dueRotateSQL, r.cfg.Rotation, after.at,
		after.principalID, after.clusterKey, r.cfg.Batch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.principalID, &d.clusterKey, &d.at)
	})
}

// RotateBroker sets a new broker password for every active principal's
// active roles not rotated within the period, through guard_role_ensure
// under the approval of the principal's last role ensure on that cluster.
// Frozen, killed and retired roles are never touched: an ensure would
// re-assert their attributes.
func (r *Runner) RotateBroker(ctx context.Context, f Fence) (Report, error) {
	return r.pass(ctx, f, JobBrokerRotation, r.dueRotations, r.rotateOne)
}

func (r *Runner) rotateOne(ctx context.Context, targets []agentguard.KillTarget,
	d due) (string, Outcome, agentguard.Cluster) {
	o := Outcome{PrincipalID: d.principalID, ClusterKey: d.clusterKey}
	cluster, ex, ok := clusterFor(targets, d.clusterKey)
	if !ok {
		o.Reason, o.Detail = ReasonClusterUnreachable, unreachable(d.clusterKey)
		return bucketSkipped, o, cluster
	}
	if err := lastEnsure(ctx, cluster, &d); err != nil {
		o.Reason, o.Detail = ReasonError, err.Error()
		return bucketFailed, o, cluster
	}
	if bucket, skip, ok := r.needApprover(ctx, o, d.approver,
		"approved its last role ensure"); !ok {
		return bucket, skip, cluster
	}
	res, err := r.roles.Ensure(ctx, agentguard.RoleRequest{PrincipalID: d.principalID,
		Cluster: cluster, Executor: ex, RotateCredential: true,
		Approval: agentguard.Approval{ApprovedBy: *d.approver, ApprovalID: d.approvalID},
		Scheduled: &agentguard.ScheduledRun{Job: JobBrokerRotation,
			OriginalApprovedBy: *d.approver, OriginalActionID: d.actionID,
			OriginalApprovalID: d.approvalID}})
	o.ActionID = res.ActionID
	bucket, o := classify(o, err)
	return bucket, o, cluster
}

const lastEnsureSQL = `/* pg_sage agent_upkeep_last_ensure v1 */
SELECT id, approved_by, COALESCE(approval_id, 0), executed_at FROM sage.action_log
WHERE principal_id = $1 AND action_type = 'guard_role_ensure' AND outcome = 'success'
  AND before_state->>'cluster_key' = $2
ORDER BY executed_at DESC, id DESC LIMIT 1`

// lastEnsure finds the newest successful guard_role_ensure of the
// principal on the cluster in any of its databases (core records it in
// the database that administered the cluster) and sets d's approver.
func lastEnsure(ctx context.Context, c agentguard.Cluster, d *due) error {
	var newest time.Time
	for _, db := range c.Databases {
		var id, approval int64
		var by *int
		var at time.Time
		err := db.Pool.QueryRow(ctx, lastEnsureSQL, d.principalID, c.Key).
			Scan(&id, &by, &approval, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading the last role ensure of %s in %s: %w", d.principalID,
				db.Name, err)
		}
		if d.actionID == 0 || at.After(newest) {
			newest, d.actionID, d.approver, d.approvalID = at, id, by, approval
		}
	}
	return nil
}
