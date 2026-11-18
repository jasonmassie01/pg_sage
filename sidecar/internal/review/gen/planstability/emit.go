package planstability

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

const (
	metricEstimate = "row_estimate_error"
	sourceCatalog  = "pg_stats, pg_attribute and pg_stat_all_tables at review time"
)

// candidates are the cliffs' steps (by column) then the extended
// statistics, each in a stable order.
func (j *judge) candidates() []candidate.Candidate {
	keys := make([]string, 0, len(j.cliffs))
	for k := range j.cliffs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []candidate.Candidate
	for _, k := range keys {
		out = append(out, j.cliffSteps(j.cliffs[k])...)
	}
	return append(out, j.stx...)
}

// targets are a column's current effective target, the raised one and
// the one its rollback restores (-1: the default).
func (j *judge) targets(c ColumnFacts) (current, next, restore int) {
	current, restore = j.facts.DefaultTarget, -1
	if current <= 0 {
		current = 100
	}
	if c.StatTarget != nil && *c.StatTarget >= 0 {
		current, restore = int(*c.StatTarget), int(*c.StatTarget)
	}
	next = current * TargetFactor
	if next > executor.MaxColumnStatisticsTarget {
		next = executor.MaxColumnStatisticsTarget
	}
	return current, next, restore
}

// cliffSteps is the statistics step and its ANALYZE, an operator step
// when pg_sage cannot run it, or advice when no raise is possible.
func (j *judge) cliffSteps(c *cliff) []candidate.Candidate {
	t := j.facts.Tables[c.table]
	current, next, restore := j.targets(t.Columns[c.column])
	schema, name, _ := strings.Cut(c.table, ".")
	base := j.base(c)
	base.Prediction.Quantities["target_from"] = float64(current)
	switch {
	case current == 0:
		return []candidate.Candidate{advice(base, KindSwitchedOff, c.why+". Its statistics "+
			"are switched off (target 0) by its owner; pg_sage leaves that choice alone.")}
	case current >= executor.MaxColumnStatisticsTarget:
		return []candidate.Candidate{advice(base, KindAtMaximum, fmt.Sprintf("%s. Its "+
			"statistics target is already the %d maximum: the fix is in the query or an "+
			"index, not the sample.", c.why, executor.MaxColumnStatisticsTarget))}
	}
	base.Prediction.Quantities["target_to"] = float64(next)
	forward, err := executor.SetColumnStatisticsSQL(schema, name, c.column, next)
	rollback, rerr := executor.SetColumnStatisticsSQL(schema, name, c.column, restore)
	if err != nil || rerr != nil {
		j.cov.Note("%s skipped: %v", c.table+"."+c.column, firstErr(err, rerr))
		return nil
	}
	analyze := analyzeSQL(schema, name, c.column)
	reason := fmt.Sprintf("%s. Raising its statistics target from %d to %d samples %d "+
		"rows instead of %d; the ANALYZE after it builds the larger sample.", c.why, current,
		next, 300*next, 300*current)
	if !t.Owned {
		return []candidate.Candidate{operatorStep(base, forward, rollback, analyze, next,
			reason+" pg_sage cannot run it (SET STATISTICS needs table ownership), so the "+
				"table owner runs it.")}
	}
	if err := executor.ValidateExecutorSQL(forward); err != nil {
		return []candidate.Candidate{operatorStep(base, forward, rollback, analyze, next,
			reason+" pg_sage's executor refuses this statement ("+err.Error()+"), so the "+
				"table owner runs it.")}
	}
	step := base
	step.ActionType, step.ForwardSQL, step.RollbackSQL = executor.ActionTypeSetColumnStatistics,
		forward, rollback
	step.Reversible, step.Reason, step.Approval = true, reason, candidate.ApprovalAutomatic
	step.ID = candidate.NewID(Name, step.Kind, step.Target, step.IDBody())
	an := base
	an.Kind, an.ActionType, an.ForwardSQL = KindAnalyze, "analyze_table", analyze
	an.Reason = "Builds the larger sample of " + step.Target + " after its statistics " +
		"target step."
	an.Approval, an.Prediction.Quantities = candidate.ApprovalAutomatic, nil
	an.ID = candidate.NewID(Name, an.Kind, an.Target, an.IDBody())
	return []candidate.Candidate{step, an}
}

// base is a cliff candidate without its action.
func (j *judge) base(c *cliff) candidate.Candidate {
	evidence := append([]string(nil), c.queries...)
	if id, ok := j.in.statIDs[c.table+"."+c.column]; ok {
		evidence = append(evidence, id)
	}
	sort.Strings(evidence)
	quantities := map[string]float64{}
	for k, v := range c.quantities {
		quantities[k] = v
	}
	return candidate.Candidate{Generator: Name, Dimension: Dimension, Kind: c.kind,
		Target: c.table + "." + c.column, EvidenceIDs: evidence,
		Prediction: candidate.Prediction{Metric: metricEstimate, Direction: "down",
			Tier: candidate.TierNone, Source: sourceCatalog, Quantities: quantities}}
}

func advice(c candidate.Candidate, kind, reason string) candidate.Candidate {
	c.Kind, c.ActionType, c.Reason = kind, candidate.ActionAdvice, reason
	c.Approval = candidate.ApprovalRequired
	c.ID = candidate.NewID(Name, c.Kind, c.Target, c.IDBody())
	return c
}

// operatorStep is the step pg_sage cannot run (no ownership, or a name
// the executor's validator refuses), with the exact SQL; reason says which.
func operatorStep(c candidate.Candidate, forward, rollback, analyze string, next int,
	reason string) candidate.Candidate {
	c.ActionType, c.Approval = candidate.ActionOperator, candidate.ApprovalRequired
	c.Reason = reason
	c.Operator = &candidate.Operator{Command: forward + "; " + analyze, Revert: rollback,
		Setting: "pg_attribute.attstattarget of " + c.Target, Value: strconv.Itoa(next)}
	c.Reversible = true
	c.ID = candidate.NewID(Name, c.Kind, c.Target, c.IDBody())
	return c
}

// analyzeSQL is the ANALYZE of the one column whose target changed.
func analyzeSQL(schema, table, column string) string {
	return "ANALYZE " + sqlident.Qualified(schema, table) + " (" + sqlident.Ident(column) + ")"
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
