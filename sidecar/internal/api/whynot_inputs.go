package api

import (
	"context"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/whynot"
)

// whyNotFindingsCap bounds the open findings one question reads.
const whyNotFindingsCap = 50

// whyNotProbeTimeout bounds the privilege probe on the monitored database.
var whyNotProbeTimeout = 3 * time.Second

// whyNotFindingsSQL reads the open findings an object or category
// question names (both optional), newest first, with findingDetailSQL's
// columns so findingDetailRow scans them.
const whyNotFindingsSQL = `/* pg_sage */SELECT id, created_at, last_seen,
 occurrence_count, category, severity, object_type,
 object_identifier, title, detail, recommendation,
 recommended_sql, rollback_sql, estimated_cost_usd,
 status, suppressed_until, resolved_at, acted_on_at,
 action_log_id
 FROM sage.findings
 WHERE status = 'open'
   AND ($1 = '' OR object_identifier = $1)
   AND ($2 = '' OR category = $2)
 ORDER BY id DESC LIMIT $3`

// readWhyNotFindings reads the finding asked about (pgx.ErrNoRows when it
// is not in this database), or the open findings the question names.
func readWhyNotFindings(
	ctx context.Context, sel namedPool, q whyNotQuery,
) ([]map[string]any, error) {
	if q.findingID > 0 {
		row, err := queryFindingByID(ctx, sel.pool, strconv.FormatInt(q.findingID, 10))
		if err != nil {
			return nil, err
		}
		return []map[string]any{row}, nil
	}
	rows, err := sel.pool.Query(ctx, whyNotFindingsSQL, q.object, q.category,
		whyNotFindingsCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var row findingDetailRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, err
		}
		out = append(out, row.toMap())
	}
	return out, rows.Err()
}

// explainWhyNotCandidates asks the standing gate about every candidate,
// through the cases view's explain path (recording nothing); a seam for
// tests. A cancelled or expired request is an error, not a verdict.
var explainWhyNotCandidates = func(
	ctx context.Context, all []cases.Case, inst *fleet.DatabaseInstance,
) ([]explainedCandidate, error) {
	details := enrichCasesActionPolicies(ctx, all, inst)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []explainedCandidate
	for i := range all {
		for j, candidate := range all[i].ActionCandidates {
			out = append(out, explainedFrom(candidate, details[i][j]))
		}
	}
	return out, nil
}

// whyNotNow is the chain's clock; a seam for tests that compare answers.
var whyNotNow = func() time.Time { return time.Now().UTC() }

// gatherWhyNot reads the chain's inputs: the fleet's view of the
// database, its open findings and their candidates' verdicts, and the
// privilege probe. A read that fails leaves its links unknown.
func gatherWhyNot(ctx context.Context, mgr *fleet.DatabaseManager, sel namedPool,
	q whyNotQuery, rows []map[string]any, readErr error) whynot.Inputs {
	inst := mgr.GetInstance(sel.name)
	in := whynot.Inputs{Now: whyNotNow(), Database: sel.name,
		Findings: whynot.Findings{Category: q.category, FindingID: q.findingID, Err: readErr}}
	applyFleetFacts(&in, mgr, inst)
	if readErr == nil {
		applyFindingFacts(ctx, &in, rows, inst)
	}
	probeCtx, cancel := context.WithTimeout(ctx, whyNotProbeTimeout)
	defer cancel()
	in.Privileges, in.PrivilegesErr = whynot.ProbePrivileges(probeCtx, sel.pool)
	return in
}

// applyFindingFacts projects the open rows into cases, counts their
// candidates and explains them.
func applyFindingFacts(ctx context.Context, in *whynot.Inputs, rows []map[string]any,
	inst *fleet.DatabaseInstance) {
	open := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if stringValue(row["status"]) == "open" {
			row["database_name"] = in.Database
			open = append(open, row)
		}
	}
	all, _ := projectFindingRows(open)
	in.Findings.Open = len(all)
	for _, c := range all {
		in.Candidates += len(c.ActionCandidates)
	}
	if in.Candidates == 0 {
		return
	}
	explained, err := explainWhyNotCandidates(ctx, all, inst)
	if err != nil {
		in.PolicyErr = err
		return
	}
	for _, c := range explained {
		in.Verdicts = append(in.Verdicts, whynot.Verdict{ActionType: c.ActionType,
			Decision: c.Decision, Reason: c.BlockedReason, Detail: c.Detail})
	}
}

// applyFleetFacts reads the collector, stop, provider, LLM and trust
// facts the fleet holds for the database.
func applyFleetFacts(in *whynot.Inputs, mgr *fleet.DatabaseManager,
	inst *fleet.DatabaseInstance) {
	overview := mgr.FleetStatus()
	in.Stop.Fleet = overview.Summary.FleetStopped
	for _, row := range overview.Databases {
		if row.Name != in.Database || row.Status == nil {
			continue
		}
		in.Observed.LastSnapshot = row.Status.CollectorLastRun
		in.Observed.Freshness = row.Status.CollectionFreshness
		in.Provider = row.Status.Capabilities.Provider
		in.Stop.Stopped, in.Stop.By = row.EmergencyStopped, row.EmergencyStoppedBy
		if row.EmergencyStoppedAt != nil {
			in.Stop.At = *row.EmergencyStoppedAt
		}
	}
	if in.Stop.Fleet && overview.Summary.FleetStoppedAt != nil {
		in.Stop.By, in.Stop.At = overview.Summary.FleetStoppedBy, *overview.Summary.FleetStoppedAt
	}
	in.Observed.Interval = collectorInterval(mgr, inst)
	if cfg := mgr.Config(); cfg != nil {
		config.RLockForHotReload()
		in.LLMEnabled = cfg.LLM.Enabled
		config.RUnlockForHotReload()
	}
	if inst != nil && inst.Executor != nil {
		in.TrustLevel = inst.Executor.TrustLevel()
	}
}

// collectorInterval is the collector's period: the running collector's,
// else the database's own, else the fleet's.
func collectorInterval(mgr *fleet.DatabaseManager, inst *fleet.DatabaseInstance) time.Duration {
	if inst != nil && inst.Collector != nil {
		if period := inst.Collector.Interval(); period > 0 {
			return period
		}
	}
	secs := 0
	if inst != nil {
		secs = inst.ConfigSnapshot().CollectorIntervalSeconds
	}
	if secs <= 0 && mgr.Config() != nil {
		config.RLockForHotReload()
		secs = mgr.Config().Collector.IntervalSeconds
		config.RUnlockForHotReload()
	}
	return time.Duration(secs) * time.Second
}
