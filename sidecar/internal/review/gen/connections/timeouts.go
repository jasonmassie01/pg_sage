package connections

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// IdleInTxLimitMs is the idle_in_transaction_session_timeout proposed for
// a role whose sessions sat idle in a transaction longer than it (10
// minutes): such a session holds its locks and the xmin horizon.
const IdleInTxLimitMs = 10 * 60 * 1000

// timeouts proposes the timeout for each role over the limit.
func (b *builder) timeouts() {
	idle := slices.Clone(b.in.IdleInTx)
	slices.SortFunc(idle, func(x, y IdleInTx) int { return strings.Compare(x.Role, y.Role) })
	for _, r := range idle {
		if r.AgeSeconds*1000 > IdleInTxLimitMs {
			b.timeout(r)
		}
	}
}

// timeout is the role's step, or advice when its facts were not read or
// its prior value cannot be set back exactly; nothing when the contract
// refuses the role or it is already bounded at the limit or tighter.
func (b *builder) timeout(r IdleInTx) {
	c := candidate.Candidate{Kind: KindIdleInTxTimeout, Target: roleTarget(r.Role),
		ActionType: candidate.ActionAdvice, EvidenceIDs: []string{b.v.hour(r.At)},
		Prediction: candidate.Prediction{Metric: "idle_in_transaction_seconds",
			Direction: "decrease", Source: "workload_sessions groups by role",
			Quantities: map[string]float64{QuantityIdleAge: r.AgeSeconds,
				QuantityTimeoutSeconds: IdleInTxLimitMs / 1000}}}
	reason := fmt.Sprintf("A session of role %s sat idle in a transaction for %.0f seconds "+
		"(at most %d minutes is the line): it holds its locks and the xmin horizon, so "+
		"vacuum cannot remove the rows it could still see. Set "+
		"idle_in_transaction_session_timeout = '%dmin' for the role in this database.",
		r.Role, r.AgeSeconds, IdleInTxLimitMs/60000, IdleInTxLimitMs/60000)
	fact, ok, why := b.roleFact(r.Role, "idle_in_transaction_session_timeout")
	switch {
	case why != "":
		return
	case !ok:
		c.Reason = reason + " pg_sage could not read the role, so this is advice, not a step."
		b.emit(c)
		return
	}
	prior, has := fact.Settings[budget.SettingIdleInTx]
	if has {
		ms, err := config.ParseBudgetDuration(prior)
		switch {
		case err != nil:
			c.Reason = fmt.Sprintf("%s The role's own value %q is not a duration pg_sage can "+
				"set back exactly, so this is advice, not a step.", reason, prior)
			b.emit(c)
			return
		case ms > 0 && ms <= IdleInTxLimitMs:
			return
		}
	}
	var priorPtr *string
	if has {
		priorPtr = &prior
	}
	c.ActionType, c.Reason = ActionRoleTimeout, reason
	c.ForwardSQL = budget.RoleSettingSQL(r.Role, b.in.Database, budget.SettingIdleInTx,
		IdleInTxLimitMs)
	c.RollbackSQL = budget.RoleSettingRollbackSQL(r.Role, b.in.Database,
		budget.SettingIdleInTx, priorPtr)
	b.emit(c)
}

// roleFact is the role's facts. ok is false when they were not read (no
// catalog, no database name). A non-empty why says no candidate may name
// the role (dropped, or refused by the role-budget contract); it is noted
// in the coverage with the role's name.
func (b *builder) roleFact(role, what string) (fact budget.RoleFact, ok bool, why string) {
	if b.in.Roles == nil || b.in.Database == "" {
		return budget.RoleFact{}, false, ""
	}
	fact, found := b.in.Roles[role]
	if !found {
		why = fmt.Sprintf("role %s no longer exists", role)
	} else if err := budget.RefusedRole(role, b.in.SelfRole, fact.Superuser); err != nil {
		why = err.Error()
	}
	if why != "" {
		b.cov.Note("no %s step for role %s: %s", what, role, why)
		return budget.RoleFact{}, false, why
	}
	return fact, true, ""
}
