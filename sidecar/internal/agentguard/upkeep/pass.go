package upkeep

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

type (
	pageFunc func(ctx context.Context, after due) ([]due, error)
	itemFunc func(ctx context.Context, targets []agentguard.KillTarget, d due) (string,
		Outcome, agentguard.Cluster)
)

// pass pages through every due row (keyset, so a row that stays due, such
// as one waiting for a person, never starves the rest) and runs item on
// each, checking the lease before every write.
func (r *Runner) pass(ctx context.Context, f Fence, job string, page pageFunc,
	item itemFunc) (Report, error) {
	var rep Report
	if err := f.check(ctx, r.control); err != nil {
		return rep, err
	}
	targets, err := r.targets(ctx)
	if err != nil {
		return rep, fmt.Errorf("upkeep: listing databases for %s: %w", job, err)
	}
	after := due{}
	for {
		rows, err := page(ctx, after)
		if err != nil {
			return rep, fmt.Errorf("upkeep: reading %s due rows (retired principals or "+
				"broker logins): %w", job, err)
		}
		for _, d := range rows {
			if err := f.check(ctx, r.control); err != nil {
				return rep, err
			}
			bucket, o, cluster := item(ctx, targets, d)
			rep.add(bucket, o)
			if ferr := keepFinding(ctx, job, bucket, o, cluster); ferr != nil {
				rep.add(bucketFailed, Outcome{PrincipalID: o.PrincipalID,
					ClusterKey: o.ClusterKey, Reason: ReasonError, Detail: ferr.Error()})
			}
		}
		if len(rows) < r.cfg.Batch {
			return rep, nil
		}
		after = rows[len(rows)-1]
	}
}

// keepFinding keeps a job's warning finding in step in every database of
// the cluster: raised while it waits for a person (no approver, or another
// grantor's residue), resolved once it is done. A cluster with no
// reachable database has nowhere to hold one; the pass reports it.
func keepFinding(ctx context.Context, job, bucket string, o Outcome,
	c agentguard.Cluster) error {
	ident := job + ":" + o.PrincipalID
	waiting := bucket == bucketIncomplete || (bucket == bucketSkipped &&
		(o.Reason == ReasonNoApprover || o.Reason == ReasonApproverInactive))
	var errs []string
	for _, db := range c.Databases {
		var err error
		switch {
		case waiting:
			err = raise(ctx, db.Pool, upkeepFinding(job, ident, o))
		case bucket == bucketDone:
			err = resolve(ctx, db.Pool, UpkeepFindingCategory, ident)
		}
		if err != nil {
			errs = append(errs, db.Name+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("upkeep: %s finding of %s: %s", job, o.PrincipalID,
			strings.Join(errs, "; "))
	}
	return nil
}

func upkeepFinding(job, ident string, o Outcome) finding {
	f := finding{category: UpkeepFindingCategory, severity: "warning",
		objectType: "agent", ident: ident, sql: o.Fix,
		detail: map[string]any{"job": job, "principal_id": o.PrincipalID,
			"cluster_key": o.ClusterKey, "reason": o.Reason, "detail": o.Detail}}
	switch job {
	case JobRetireGrace:
		f.title = fmt.Sprintf("Retired agent %s still has its roles on %s: %s",
			o.PrincipalID, o.ClusterKey, o.Reason)
		f.recommendation = "pg_sage drops a retired agent's roles only under the approval " +
			"of the admin who retired it, and never forces a revoke. " + o.Detail + ". "
		if o.Fix != "" {
			f.recommendation += "Another grantor's privileges remain: that grantor runs " +
				"the statements shown, then the next pass drops the roles."
		} else {
			f.recommendation += "pg_sage refuses the retired agent's requests, but its " +
				"roles still exist; drop them by hand if they must go now."
		}
	default:
		f.title = fmt.Sprintf("Broker password of agent %s on %s is not rotating: %s",
			o.PrincipalID, o.ClusterKey, o.Reason)
		f.recommendation = "pg_sage rotates a broker password under the approval of the " +
			"last role ensure for the agent. " + o.Detail + ". Approve a new role ensure " +
			"for this agent; rotation resumes under it."
	}
	return f
}
