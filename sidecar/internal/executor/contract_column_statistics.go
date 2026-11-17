package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// ActionTypeSetColumnStatistics raises (or restores) one column's planner
// statistics target (review-02c). It is the one ALTER TABLE ... ALTER
// COLUMN ... SET STATISTICS clause; every other ALTER TABLE stays the
// high-risk alter_table.
const ActionTypeSetColumnStatistics = "set_column_statistics"

// The targets the contract sets: 1..MaxColumnStatisticsTarget, or -1
// (the default_statistics_target; NULL in pg_attribute on PostgreSQL 17+).
// 0 switches the column's statistics off, which pg_sage never does.
const (
	MaxColumnStatisticsTarget = 10000
	defaultColumnStatTarget   = -1
)

func setColumnStatisticsContract() ActionContract {
	return ActionContract{
		ActionType:          ActionTypeSetColumnStatistics,
		BaseRiskTier:        "moderate",
		ProviderSupport:     portableActionProviders(),
		RequiredPermissions: []string{"table ownership (or MAINTAIN on PostgreSQL 17+)"},
		Prechecks: []string{
			"column exists and is not dropped",
			"prior pg_attribute.attstattarget captured for the rollback",
		},
		Guardrails: []string{
			"one ALTER COLUMN ... SET STATISTICS clause, target 1..10000 or -1 (default)",
			"SHARE UPDATE EXCLUSIVE on the table",
			"lock_timeout",
			"statement_timeout",
			"identifiers quoted by sqlident",
		},
		ExecutionPlan: []string{
			"ALTER TABLE schema.table ALTER COLUMN column SET STATISTICS n",
			"the next ANALYZE (a separate analyze_table step) builds the larger sample",
		},
		SuccessCriteria: []string{
			"pg_attribute.attstattarget reads the requested target",
			"the target query's row estimate moves toward its actual rows",
		},
		PostChecks: []string{"read back pg_attribute.attstattarget"},
		// SET STATISTICS <previous> (-1 for the default) undoes it exactly.
		RollbackClass: "reversible",
		Cooldown:      "configured rollback cooldown",
		AuditFields:   []string{"table", "column", "target", "prior_target", "case_id"},
	}
}

// ColumnStatisticsStmt is a parsed SET STATISTICS. Table is as written
// (quotes kept); Column is the name PostgreSQL resolves (bare names folded
// to lower case, quoted ones unescaped).
type ColumnStatisticsStmt struct {
	Table  string
	Column string
	Target int
}

const statIdent = `(?:"(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)`

var setColumnStatisticsPattern = regexp.MustCompile(`(?i)^ALTER TABLE (?:IF EXISTS )?` +
	`(?:ONLY )?(` + statIdent + `(?:\.` + statIdent + `)?) ALTER (?:COLUMN )?(` + statIdent +
	`) SET STATISTICS (-?[0-9]{1,6}) ?;?$`)

// ParseSetColumnStatistics reads one ALTER TABLE ... ALTER [COLUMN] c SET
// STATISTICS <integer>. A list of subcommands, a second statement or any
// other value (DEFAULT included) does not match. The target's range is
// checked by ValidateExecutorSQL, not here.
func ParseSetColumnStatistics(sql string) (ColumnStatisticsStmt, bool) {
	m := setColumnStatisticsPattern.FindStringSubmatch(normalizeSQLText(sql))
	if m == nil {
		return ColumnStatisticsStmt{}, false
	}
	target, err := strconv.Atoi(m[3])
	if err != nil {
		return ColumnStatisticsStmt{}, false
	}
	return ColumnStatisticsStmt{Table: m[1], Column: resolvedIdent(m[2]), Target: target}, true
}

// resolvedIdent is an identifier as PostgreSQL resolves it.
func resolvedIdent(raw string) string {
	if strings.HasPrefix(raw, `"`) {
		return strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
	}
	return strings.ToLower(raw)
}

// validColumnStatTarget is a target the contract sets.
func validColumnStatTarget(n int) bool {
	return n == defaultColumnStatTarget || n >= 1 && n <= MaxColumnStatisticsTarget
}

// checkColumnStatisticsSQL is the validator's range check.
func checkColumnStatisticsSQL(stmt ColumnStatisticsStmt) error {
	if !validColumnStatTarget(stmt.Target) {
		return fmt.Errorf("%w: SET STATISTICS %d: the target must be 1..%d, or -1 for the "+
			"default", ErrDisallowedSQL, stmt.Target, MaxColumnStatisticsTarget)
	}
	return nil
}

// SetColumnStatisticsSQL is the statement setting schema.table.column's
// target, every name quoted by sqlident.
func SetColumnStatisticsSQL(schema, table, column string, target int) (string, error) {
	if schema == "" || table == "" || column == "" {
		return "", fmt.Errorf("set column statistics: schema %q, table %q and column %q "+
			"must all be named", schema, table, column)
	}
	if !validColumnStatTarget(target) {
		return "", fmt.Errorf("set column statistics on %s.%s.%s: target %d is not "+
			"1..%d or -1", schema, table, column, target, MaxColumnStatisticsTarget)
	}
	return columnStatisticsSQL(sqlident.Qualified(schema, table), column, target), nil
}

func columnStatisticsSQL(qualified, column string, target int) string {
	return "ALTER TABLE " + qualified + " ALTER COLUMN " + sqlident.Ident(column) +
		" SET STATISTICS " + strconv.Itoa(target)
}

// columnStatisticsRollbackSQL restores the prior target: -1 for the
// default (-1, or NULL on PostgreSQL 17+). A prior the contract never sets
// (0: statistics off) has no faithful rollback.
func columnStatisticsRollbackSQL(stmt ColumnStatisticsStmt, prior *int16) (string, error) {
	target := defaultColumnStatTarget
	if prior != nil {
		target = int(*prior)
	}
	if !validColumnStatTarget(target) {
		return "", fmt.Errorf("%w: %s.%s has statistics target %d, which pg_sage never "+
			"sets back", ErrNoFaithfulRollback, stmt.Table, stmt.Column, target)
	}
	return columnStatisticsSQL(stmt.Table, stmt.Column, target), nil
}

// statTargetMatches compares a read-back attstattarget with the request.
func statTargetMatches(want int, live *int16) bool {
	if live == nil {
		return want == defaultColumnStatTarget
	}
	return int(*live) == want
}

// captureColumnStatTarget reads the column's attstattarget (nil: NULL).
func captureColumnStatTarget(
	ctx context.Context, pool *pgxpool.Pool, stmt ColumnStatisticsStmt,
) (*int16, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: database pool unavailable", ErrConfigPrior)
	}
	var target *int16
	err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT a.attstattarget::int2
		FROM pg_attribute a WHERE a.attrelid = to_regclass($1) AND a.attname = $2
		  AND a.attnum > 0 AND NOT a.attisdropped`, stmt.Table, stmt.Column).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s has no column %q", ErrConfigPrior, stmt.Table,
			stmt.Column)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: statistics target of %s.%s: %v", ErrConfigPrior,
			stmt.Table, stmt.Column, err)
	}
	return target, nil
}
