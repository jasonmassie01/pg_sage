package changeplan

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Steps that are never automatic at any trust level (plan-02b, spec
// section 3). Approve-all (and, from plan-03a, approve-up-to-N) never
// covers such a step: the engine stops before it with awaiting_approval
// until a person approves that step on its own.

// Why a step needs its own approval (the reason's prefix).
const (
	ReasonRollbackClass   = "rollback_class"
	ReasonRollbackMissing = "rollback_missing"
	ReasonNoContract      = "no_contract"
	ReasonTableRewrite    = "table_rewrite"
	ReasonPartition       = "partition_conversion"
	ReasonTypeChange      = "type_change"
	ReasonDroppedData     = "dropped_data"
	ReasonRestartBound    = "restart_bound_setting"
	ReasonSettingUnknown  = "setting_context_unknown"
	ReasonTopology        = "instance_or_topology_change"
	ReasonPrivilegeRevoke = "privilege_revoke"
	ReasonPoolerMode      = "pooler_mode_change"
	ReasonUnclassifiable  = "unclassifiable"
)

// SettingContexts maps a setting's name (lower case) to its
// pg_settings.context on the monitored database.
type SettingContexts map[string]string

var (
	reDroppedData = regexp.MustCompile(`(?is)(^|;)\s*(DROP\s+(TABLE|SCHEMA|DATABASE|` +
		`MATERIALIZED\s+VIEW)\b|TRUNCATE\b|DELETE\s+FROM\b)|\bDROP\s+COLUMN\b`)
	reTypeChange = regexp.MustCompile(`(?is)\bALTER\s+(COLUMN\s+)?("[^"]+"|\w+)\s+` +
		`(SET\s+DATA\s+)?TYPE\b`)
	rePartition = regexp.MustCompile(`(?is)\b(ATTACH|DETACH)\s+PARTITION\b|` +
		`\bPARTITION\s+BY\b|\bPARTITION\s+OF\b`)
	reRewrite = regexp.MustCompile(`(?is)\bVACUUM\s+(\([^)]*\bFULL\b[^)]*\)|FULL\b)|` +
		`(^|;)\s*CLUSTER\b|\bSET\s+(TABLESPACE|ACCESS\s+METHOD|LOGGED|UNLOGGED)\b`)
	reTopologySQL = regexp.MustCompile(`(?is)\bpg_promote\s*\(|` +
		`\bpg_(create|drop)_(physical|logical)?_?replication_slot\s*\(|` +
		`\b(CREATE|ALTER|DROP)\s+(SUBSCRIPTION|PUBLICATION)\b|` +
		`\b(primary_conninfo|primary_slot_name|synchronous_standby_names)\b`)
	reTopologyType = regexp.MustCompile(`(?i)(^|_)(failover|switchover|replicas?|resize|` +
		`instance|restart|topology|tier)(_|$)`)
	reRevoke = regexp.MustCompile(`(?is)(^|;)\s*REVOKE\b`)
	rePooler = regexp.MustCompile(`(?i)pool_mode|pooler`)
	// reSettingName reads the setting an ALTER SYSTEM, ALTER DATABASE or
	// ALTER ROLE / USER statement sets or resets.
	reSettingName = regexp.MustCompile(`(?is)^\s*ALTER\s+(?:SYSTEM|(?:DATABASE|ROLE|USER)` +
		`\s+.*?)\s+(?:SET|RESET)\s+"?([a-z_][a-z0-9_.]*)"?`)
)

// NeedsOwnApproval reports whether step s is never automatic, with why
// ("<reason>: detail"). settings holds the contexts of the settings the
// step changes; a setting step whose context is missing needs its own
// approval (fail closed). Check and operator steps are never run by the
// executor, so they are not classified.
func NeedsOwnApproval(s Step, settings SettingContexts) (bool, string) {
	if s.Kind != KindAction {
		return false, ""
	}
	if why := sqlClass(s.ProposedSQL); why != "" {
		return true, why
	}
	if reTopologyType.MatchString(s.ActionType) {
		return true, fmt.Sprintf("%s: action type %s changes the instance or its topology",
			ReasonTopology, s.ActionType)
	}
	if rePooler.MatchString(s.ActionType) {
		return true, fmt.Sprintf("%s: action type %s changes a connection pooler",
			ReasonPoolerMode, s.ActionType)
	}
	if why := settingClass(s, settings); why != "" {
		return true, why
	}
	return contractClass(s)
}

// sqlClass is the never-automatic class of sql's text ("" for none),
// from the migration statement classifier and the statement forms it does
// not cover (DML, REVOKE, replication, the pooler).
func sqlClass(sql string) string {
	res, _ := migration.ClassifyStatements(context.Background(), nil, sql, 0)
	if res.Untokenizable {
		return ReasonUnclassifiable + ": the SQL ends inside a literal or comment"
	}
	var rules []string
	rewrite := false
	for _, c := range res.All() {
		rules = append(rules, c.RuleID)
		rewrite = rewrite || c.RequiresRewrite
	}
	joined := " " + strings.Join(rules, " ") + " "
	has := func(ids ...string) bool {
		for _, id := range ids {
			if strings.Contains(joined, " "+id+" ") {
				return true
			}
		}
		return false
	}
	switch {
	case has("ddl_drop_table", "ddl_drop_column", "drop_column") ||
		reDroppedData.MatchString(sql):
		return ReasonDroppedData + ": the step drops or deletes data"
	case has("ddl_alter_type_rewrite", "alter_column_type") || reTypeChange.MatchString(sql):
		return ReasonTypeChange + ": the step changes a column's type"
	case has("ddl_attach_partition_no_check", "ddl_detach_partition_not_concurrent") ||
		rePartition.MatchString(sql):
		return ReasonPartition + ": the step converts or reshapes partitions"
	case rewrite || has("ddl_vacuum_full", "ddl_cluster", "ddl_set_tablespace",
		"ddl_set_access_method", "ddl_set_logged") || reRewrite.MatchString(sql):
		return ReasonTableRewrite + ": the step rewrites a table"
	case reTopologySQL.MatchString(sql):
		return ReasonTopology + ": the step changes replication or the instance's role"
	case reRevoke.MatchString(sql):
		return ReasonPrivilegeRevoke + ": the step revokes a privilege"
	case rePooler.MatchString(sql):
		return ReasonPoolerMode + ": the step changes a pooler's mode"
	}
	return ""
}

// settingClass is why a setting step needs its own approval: a setting it
// touches (the one its SQL sets and the one its readback names) is
// restart-bound (pg_settings.context postmaster, the change the executor
// records as applied_pending_restart) or of unknown context.
func settingClass(s Step, settings SettingContexts) string {
	if !isSetting(s) {
		return ""
	}
	names := stepSettingNames(s)
	if len(names) == 0 {
		return ReasonSettingUnknown + ": the step does not name one setting"
	}
	unknown := ""
	for _, name := range names {
		settingContext, ok := settings[name]
		switch {
		case name == "all":
			return ReasonSettingUnknown + ": the step does not name one setting"
		case !ok && unknown == "":
			unknown = fmt.Sprintf("%s: the context of setting %s is unknown",
				ReasonSettingUnknown, name)
		case settingContext == "postmaster":
			return fmt.Sprintf("%s: %s takes effect only after a restart", ReasonRestartBound,
				name)
		}
	}
	return unknown
}

// contractClass is the rule on the executor contracts the step runs under
// (its action type and the one the executor derives from its SQL): each
// must undo cleanly (reversible, with its rollback SQL) or need no undo.
func contractClass(s Step) (bool, string) {
	types := []string{s.ActionType, executor.ActionTypeForSQL(s.ProposedSQL)}
	for _, actionType := range types {
		contract, ok := executor.ContractForActionType(actionType)
		if !ok {
			return true, fmt.Sprintf("%s: the executor has no contract for action type %q "+
				"of this SQL", ReasonNoContract, actionType)
		}
		switch class := policy.RollbackClass(contract.RollbackClass); class {
		case policy.RollbackNoRollbackNeeded:
		case policy.RollbackReversible:
			if strings.TrimSpace(s.RollbackSQL) == "" {
				return true, fmt.Sprintf("%s: %s is reversible but the step has no "+
					"rollback SQL", ReasonRollbackMissing, actionType)
			}
		default:
			return true, fmt.Sprintf("%s: %s is %s", ReasonRollbackClass, actionType, class)
		}
	}
	return false, ""
}

// stepSettingNames are the settings step s touches (lower case, sorted,
// distinct): the one its SQL sets or resets and the one its Setting reads
// back. Both are classified, so a readback naming another setting never
// hides what the SQL changes.
func stepSettingNames(s Step) []string {
	var names []string
	if m := reSettingName.FindStringSubmatch(s.ProposedSQL); m != nil {
		names = append(names, strings.ToLower(m[1]))
	}
	if s.Setting != "" {
		names = append(names, strings.ToLower(s.Setting))
	}
	if s.Landing != nil && s.Landing.Setting != "" {
		names = append(names, strings.ToLower(s.Landing.Setting))
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// readSettingContexts reads the contexts of the settings p's steps
// change from pg_settings (read-only, bounded by q's statement timeout).
func readSettingContexts(ctx context.Context, q querier, steps []Step) (SettingContexts,
	error) {
	var names []string
	for _, s := range steps {
		if (s.Kind == KindAction || s.Kind == KindOperator) && isSetting(s) {
			names = append(names, stepSettingNames(s)...)
		}
	}
	out := SettingContexts{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `/* pg_sage */ SELECT name, context FROM pg_settings
		WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("read the settings' contexts: %w", err)
	}
	pairs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var p [2]string
		return p, r.Scan(&p[0], &p[1])
	})
	if err != nil {
		return nil, fmt.Errorf("read the settings' contexts: %w", err)
	}
	for _, p := range pairs {
		out[strings.ToLower(p[0])] = p[1]
	}
	return out, nil
}
