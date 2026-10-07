package approvalcard

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/store"
)

// lockByType is the lock (or other immediate impact) each action type
// takes while it runs.
var lockByType = map[string]string{
	"create_index_concurrently": "SHARE UPDATE EXCLUSIVE on the table: reads and " +
		"writes continue while the index builds",
	"reindex_concurrently": "SHARE UPDATE EXCLUSIVE on the table: reads and writes " +
		"continue during the rebuild",
	"drop_unused_index": "brief ACCESS EXCLUSIVE on the index (CONCURRENTLY waits for " +
		"running transactions instead of blocking them)",
	"revert_created_index": "brief ACCESS EXCLUSIVE on the index",
	"replace_index": "SHARE UPDATE EXCLUSIVE on the table while the new index builds " +
		"(reads and writes continue); then a brief ACCESS EXCLUSIVE on the old index " +
		"only (DROP INDEX CONCURRENTLY waits for running transactions instead of " +
		"blocking them)",
	"analyze_table":        "SHARE UPDATE EXCLUSIVE on the table: reads and writes continue",
	"vacuum_table":         "SHARE UPDATE EXCLUSIVE on the table: reads and writes continue",
	"set_table_autovacuum": "SHARE UPDATE EXCLUSIVE on the table, briefly",
	"create_statistics": "SHARE UPDATE EXCLUSIVE on the table while the statistics are " +
		"created and the table is analyzed: reads and writes continue",
	"revert_created_statistics": "SHARE UPDATE EXCLUSIVE on the table, briefly",
	"alter_table": "ACCESS EXCLUSIVE on the table: blocks reads and writes; " +
		"some changes rewrite the table",
	"alter_database_guc": "no table locks; applies to new sessions of the database",
	"cancel_backend":     "cancels one query; its transaction rolls back; no locks taken",
	"terminate_backend":  "ends one session; its transaction rolls back",
	"retention_delete":   "ROW EXCLUSIVE on the table while rows are deleted in batches",
	"apply_query_hint":   "no locks; changes the plan of the hinted query",
	"retire_query_hint":  "no locks; the query returns to its default plan",
}

// lockFor describes the lock of an action; ALTER SYSTEM depends on
// whether the setting needs a restart.
func lockFor(actionType, sql string) string {
	if actionType == "alter_system_guc" {
		if stmt, ok := pgconf.ParseAlterSystem(sql); ok && pgconf.RequiresRestart(stmt.Name) {
			return "no table locks; takes effect only after a server restart"
		}
		return "no table locks; applied by a configuration reload"
	}
	return lockByType[actionType]
}

// rollbackOf is how the change is undone.
func rollbackOf(a store.QueuedAction, contract *executor.ActionContract) Rollback {
	r := Rollback{SQL: a.RollbackSQL}
	if contract != nil {
		r.Class = contract.RollbackClass
	}
	if strings.TrimSpace(r.SQL) != "" {
		return r
	}
	switch r.Class {
	case "no_rollback_needed":
		r.Note = "Nothing to undo: the action changes no schema or setting"
	case "forward_fix_only":
		r.Note = "Cannot be undone automatically: a forward fix is needed"
	default:
		r.Note = "No rollback SQL was recorded for this action"
	}
	return r
}

// riskOf is the risk tier, blast radius, lock, guardrails and checks.
func riskOf(in Inputs, c Card) Risk {
	r := Risk{Tier: in.Action.ActionRisk, Lock: lockFor(c.ActionType, c.SQL),
		Guardrails: []string{}, PostChecks: []string{}}
	if in.Decision != nil && in.Decision.RiskTier != "" {
		r.Tier = in.Decision.RiskTier
	}
	if r.Tier == "" && in.Contract != nil {
		r.Tier = in.Contract.BaseRiskTier
	}
	if r.Tier == "" {
		r.Tier = "unknown"
	}
	r.Guardrails = appendUnique(r.Guardrails, in.Action.Guardrails...)
	if in.Decision != nil {
		r.Guardrails = appendUnique(r.Guardrails, in.Decision.Guardrails...)
	}
	if in.Contract != nil {
		r.Guardrails = appendUnique(r.Guardrails, in.Contract.Guardrails...)
		r.PostChecks = append(r.PostChecks, in.Contract.PostChecks...)
	}
	r.BlastRadius = blastRadius(c)
	return r
}

func blastRadius(c Card) string {
	var parts []string
	switch n := len(c.Targets); n {
	case 0:
		parts = append(parts, "target not recorded")
	case 1:
		parts = append(parts, "1 object: "+c.Targets[0])
	default:
		parts = append(parts, fmt.Sprintf("%d objects: %s", n, strings.Join(c.Targets, ", ")))
	}
	if size := c.Predicted.EstimatedSizeBytes; size != nil {
		parts = append(parts, "about "+formatBytes(*size)+" to build")
	}
	if n := len(c.Predicted.AffectedQueries); n > 0 {
		parts = append(parts, fmt.Sprintf("%d affected quer%s", n, plural(n, "y", "ies")))
	}
	return strings.Join(parts, "; ")
}

func appendUnique(list []string, items ...string) []string {
	for _, it := range items {
		dup := strings.TrimSpace(it) == ""
		for _, have := range list {
			dup = dup || have == it
		}
		if !dup {
			list = append(list, it)
		}
	}
	return list
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", value, "KMGTP"[exp])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
