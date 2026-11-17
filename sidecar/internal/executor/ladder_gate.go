package executor

import (
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The verification ladder's entry rule (lab-03a): every action class says
// whether it is cheap to revert. A class that is not runs unattended only
// after a passed tier-2 (clone) test; without one it is queued for a
// person at every trust level, with "not rehearsed on a clone" on its
// card. Other gates are unchanged: a tier-2 pass removes no guardrail.

type revertCost int

const (
	// revertCheap: undone quickly and exactly (drop the new index, reset
	// the setting, remove the hint).
	revertCheap revertCost = iota
	// revertCostly: undoing it costs a rebuild, a restart or a rewrite.
	revertCostly
	// revertNotGated: not a change the ladder tests (a diagnosis, an
	// incident mitigation, a containment, or a revert to the prior state).
	revertNotGated
)

// The classes by revert cost; together they cover every contract class
// (a test pins it). A class in none of them is treated as costly.
var (
	cheapClasses = []string{"analyze_table", "vacuum_table", "set_table_autovacuum",
		"reindex_concurrently", "promote_role_work_mem", ActionTypeSetRoleTimeout,
		ActionTypeSetRoleConnectionLimit, "apply_query_hint", "retire_query_hint",
		"create_statistics", "revert_created_statistics", "create_index_concurrently",
		"alter_system_guc", "alter_database_guc", ActionTypeSetColumnStatistics}
	costlyClasses = []string{"drop_unused_index", ActionTypeReplaceIndex, "alter_table",
		"retention_delete"}
	notGatedClasses = []string{"diagnose_lock_blockers", "diagnose_runaway_query",
		"diagnose_connection_exhaustion", "diagnose_wal_replication",
		"diagnose_standby_conflicts", "diagnose_freeze_blockers", "diagnose_vacuum_pressure",
		"prepare_sequence_capacity_migration", "prepare_query_rewrite",
		"prepare_parameterized_query", "investigate_query_plan", "plan_bloat_remediation",
		"ddl_preflight", "cancel_backend", "terminate_backend", "revert_created_index",
		ActionTypeRestoreDroppedIndex, ActionTypeGuardRoleEnsure, ActionTypeGuardRoleRetire,
		ActionTypeGuardFreeze, ActionTypeGuardKill, ActionTypeGuardUnfreeze,
		ActionTypeGuardGrant, ActionTypeGuardRevoke}
)

// revertCostByClass is read-only after package initialisation.
var revertCostByClass = func() map[string]revertCost {
	out := map[string]revertCost{}
	for cost, classes := range [][]string{cheapClasses, costlyClasses, notGatedClasses} {
		for _, class := range classes {
			out[class] = revertCost(cost)
		}
	}
	return out
}()

// costlyReasons say why a costly class is costly to revert.
var costlyReasons = map[string]string{
	"drop_unused_index":    "undoing an index drop costs a rebuild",
	ActionTypeReplaceIndex: "a replace drops the old index, and undoing that costs a rebuild",
	"alter_table":          "an ALTER TABLE can rewrite the table or change a column type",
	"retention_delete":     "deleted rows cannot be put back",
}

// RevertCost reports whether an action of actionType (with its SQL) may
// run without a tier-2 test, and when not, why it is costly to revert. A
// setting that needs a restart is costly; an unknown class is too.
func RevertCost(actionType, sql string) (cheap bool, why string) {
	cost, ok := revertCostByClass[actionType]
	switch {
	case !ok:
		return false, "pg_sage does not know how cheaply this kind of change is undone"
	case cost == revertCostly:
		return false, costlyReasons[actionType]
	case actionType == "alter_system_guc" || actionType == "alter_database_guc":
		stmt, parsed := configStatement(sql)
		if !parsed {
			return false, "the setting this change makes could not be read"
		}
		if pgconf.RequiresRestart(stmt.Name) {
			return false, stmt.Name + " needs a restart, so undoing it needs one too"
		}
	}
	return true, ""
}

// NotRehearsedReason is why an action needs a person under the entry
// rule ("" when it may run unattended): it is costly to revert and its
// ladder has no passed tier 2.
func NotRehearsedReason(actionType, sql string, ladder []verify.LadderRecord) string {
	cheap, why := RevertCost(actionType, sql)
	if cheap || verify.PassedTier(ladder, verify.TierClone) {
		return ""
	}
	return "not rehearsed on a clone: " + why
}

// requireApprovalWithoutRehearsal adds the approval guardrail to a costly
// class whose finding carries no passed tier-2 record.
func requireApprovalWithoutRehearsal(f analyzer.Finding, contract *policy.ActionContract) {
	if contract == nil || slices.Contains(contract.Guardrails,
		policy.GuardrailApprovalRequired) {
		return
	}
	ladder := findingLadder(f, contract.ActionType, time.Now())
	if NotRehearsedReason(contract.ActionType, f.RecommendedSQL, ladder) == "" {
		return
	}
	contract.Guardrails = append(contract.Guardrails, policy.GuardrailApprovalRequired)
}
