package plan

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/migration"
)

// lockRank orders PostgreSQL's table lock modes, weakest first.
var lockRank = map[string]int{
	LockNone: 0, "ACCESS SHARE": 1, "ROW SHARE": 2, "ROW EXCLUSIVE": 3,
	"SHARE UPDATE EXCLUSIVE": 4, "SHARE": 5, "SHARE ROW EXCLUSIVE": 6,
	"EXCLUSIVE": 7, lockAccessExclusive: 8,
}

// classifySwapSteps runs every step through migration-04's classifier
// and records what it knows. With the SQL parser, a DDL step the
// classifier does not know, or a rule that reports a stronger lock than
// the step states, is a planner bug: the plan is refused rather than
// shown with a lock level nothing confirmed. Without the parser
// (Degraded) the classifier cannot confirm most forms, so the plan is
// kept and the steps are marked not known.
func classifySwapSteps(ctx context.Context, plan *SwapPlan, version int) error {
	for i := range plan.Steps {
		s := &plan.Steps[i]
		res, err := migration.ClassifyStatements(ctx, nil, s.SQL, version)
		if err != nil {
			return fmt.Errorf("classify column swap step %s: %w", s.Name, err)
		}
		plan.Degraded = res.Degraded
		s.Known = !res.Untokenizable && len(res.Statements) == 1 && res.Unrecognized() == 0
		strongest := LockNone
		for _, c := range res.All() {
			if c.RuleID == "ddl_missing_lock_timeout" {
				continue // advice for the step alone; the script sets it first
			}
			s.Rules = append(s.Rules, c.RuleID)
			if lockRank[c.LockLevel] > lockRank[strongest] {
				strongest = c.LockLevel
			}
		}
		if res.Degraded {
			continue
		}
		if !s.Known && !s.DML {
			return fmt.Errorf("column swap step %s is not a form the DDL classifier "+
				"knows: %s", s.Name, s.SQL)
		}
		if lockRank[strongest] > lockRank[s.LockLevel] {
			return fmt.Errorf("column swap step %s states %s but the classifier "+
				"reports %s (%s)", s.Name, s.LockLevel, strongest, strings.Join(s.Rules, ", "))
		}
	}
	return nil
}
