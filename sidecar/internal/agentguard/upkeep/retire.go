package upkeep

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
)

// RecordRetiringAdmin records the admin who is retiring a principal; the
// API calls it just before setting the status to retired. A retired
// principal keeps the admin who retired it (ErrRetired).
func RecordRetiringAdmin(ctx context.Context, pool *pgxpool.Pool, principalID string,
	userID int) error {
	if pool == nil || userID <= 0 {
		return fmt.Errorf("%w: a control database and the retiring admin are required",
			ErrInvalid)
	}
	if !agentguard.ValidID(principalID) {
		return fmt.Errorf("%w: principal %q", agentguard.ErrNotFound, principalID)
	}
	var id, status string
	err := pool.QueryRow(ctx, `/* pg_sage agent_upkeep_retired_by v1 */
		WITH cur AS (SELECT id, status FROM sage.guard_principals WHERE id = $1),
		upd AS (UPDATE sage.guard_principals p SET retired_by = $2
			FROM cur WHERE p.id = cur.id AND cur.status <> 'retired' RETURNING p.id)
		SELECT cur.id, cur.status FROM cur`, principalID, userID).Scan(&id, &status)
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: principal %s", agentguard.ErrNotFound, principalID)
	case errors.As(err, &pg) && pg.Code == "23503":
		return fmt.Errorf("%w: user %d does not exist", ErrInvalid, userID)
	case err != nil:
		return fmt.Errorf("upkeep: recording the retiring admin of %s: %w", principalID, err)
	case status == string(agentguard.StatusRetired):
		return fmt.Errorf("%w: principal %s", agentguard.ErrRetired, principalID)
	}
	return nil
}

// due is one principal's roles on one cluster a job is due to act on.
type due struct {
	principalID string
	clusterKey  string
	at          time.Time
	approver    *int  // retired_by, or the last ensure's approver
	actionID    int64 // the approving action, when there is one
	approvalID  int64
}

const dueRetireSQL = `/* pg_sage agent_upkeep_retire_due v1 */
SELECT p.id, r.cluster_key, p.updated_at, p.retired_by
FROM sage.guard_principals p
JOIN sage.guard_cluster_roles r ON r.principal_id = p.id
WHERE p.status = 'retired' AND p.updated_at <= now() - $1::interval
  AND r.status <> 'retired'
  AND (p.updated_at, p.id, r.cluster_key) > ($2::timestamptz, $3::text, $4::text)
ORDER BY p.updated_at, p.id, r.cluster_key
LIMIT $5`

func (r *Runner) dueRetirements(ctx context.Context, after due) ([]due, error) {
	rows, err := r.control.Query(ctx, dueRetireSQL, r.cfg.RetireGrace, after.at,
		after.principalID, after.clusterKey, r.cfg.Batch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.principalID, &d.clusterKey, &d.at, &d.approver)
	})
}

// DropRetired drops the roles of every principal retired longer than the
// grace, on every cluster, under the retiring admin's approval.
func (r *Runner) DropRetired(ctx context.Context, f Fence) (Report, error) {
	return r.pass(ctx, f, JobRetireGrace, r.dueRetirements, r.retireOne)
}

func (r *Runner) retireOne(ctx context.Context, targets []agentguard.KillTarget,
	d due) (string, Outcome, agentguard.Cluster) {
	o := Outcome{PrincipalID: d.principalID, ClusterKey: d.clusterKey}
	cluster, ex, ok := clusterFor(targets, d.clusterKey)
	if !ok {
		o.Reason, o.Detail = ReasonClusterUnreachable, unreachable(d.clusterKey)
		return bucketSkipped, o, cluster
	}
	if bucket, skip, ok := r.needApprover(ctx, o, d.approver, "retired it"); !ok {
		return bucket, skip, cluster
	}
	res, err := r.roles.Retire(ctx, agentguard.RoleRequest{PrincipalID: d.principalID,
		Cluster: cluster, Executor: ex,
		Approval: agentguard.Approval{ApprovedBy: *d.approver},
		Scheduled: &agentguard.ScheduledRun{Job: JobRetireGrace,
			OriginalApprovedBy: *d.approver}})
	o.ActionID = res.ActionID
	bucket, o := classify(o, err)
	return bucket, o, cluster
}

func unreachable(key string) string {
	return "no monitored database of cluster " + key + " has an executor; the job runs " +
		"once one is connected"
}

// needApprover checks the approving user still exists as an operator or
// admin; ok is false with the skip outcome otherwise.
func (r *Runner) needApprover(ctx context.Context, o Outcome, approver *int,
	who string) (string, Outcome, bool) {
	if approver == nil {
		o.Reason = ReasonNoApprover
		o.Detail = "no approval is on record from the person who " + who +
			" (recorded before this release, or that user was deleted)"
		return bucketSkipped, o, false
	}
	var active bool
	err := r.control.QueryRow(ctx, `/* pg_sage agent_upkeep_approver_active v1 */
		SELECT EXISTS (SELECT 1 FROM sage.users WHERE id = $1
		AND role IN ('admin', 'operator'))`, *approver).Scan(&active)
	if err != nil {
		o.Reason, o.Detail = ReasonError, "reading the approving user: "+err.Error()
		return bucketFailed, o, false
	}
	if !active {
		o.Reason = ReasonApproverInactive
		o.Detail = fmt.Sprintf("user %d, who %s, is no longer an operator or admin", *approver,
			who)
		return bucketSkipped, o, false
	}
	return "", o, true
}
