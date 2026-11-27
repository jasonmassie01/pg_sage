package startup

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Capability keys of CheckResult.CapabilityErrors.
const (
	CapabilityWALColumns      = "wal_columns"
	CapabilityPlanTimeColumns = "plan_time_columns"
)

// capabilityEffects says what an unknown capability turns off.
var capabilityEffects = map[string]string{
	CapabilityWALColumns: "per-query WAL usage (wal_records, wal_fpi, wal_bytes) is " +
		"not collected until restart",
	CapabilityPlanTimeColumns: "per-query planning time (total_plan_time) is not " +
		"collected until restart",
}

// CheckResult holds the results of prerequisite checks.
type CheckResult struct {
	PGVersionNum int
	// QueryTextVisible is pg_read_all_stats membership with INHERIT (or
	// superuser): whether every role's query text is readable.
	QueryTextVisible   bool
	HasWALColumns      bool
	HasPlanTimeColumns bool
	// StatementsSchema is the schema pg_stat_statements was created in;
	// its readers qualify it, whatever the search_path.
	StatementsSchema string
	// CapabilityErrors holds the capability checks that could not run, by
	// key, with the cause. Such a capability is unknown and kept off: false
	// without an error means absent.
	CapabilityErrors map[string]error
}

// CapabilityWarnings describes each capability check that could not run,
// with its cause and its effect, sorted by key.
func (r *CheckResult) CapabilityWarnings() []string {
	if r == nil || len(r.CapabilityErrors) == 0 {
		return nil
	}
	keys := make([]string, 0, len(r.CapabilityErrors))
	for k := range r.CapabilityErrors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("could not check %s: %v; %s", k,
			r.CapabilityErrors[k], capabilityEffects[k]))
	}
	return out
}

// checkCapabilityColumns sets the WAL and plan-time column flags. A check
// that fails is recorded in CapabilityErrors with its flag off; only the
// caller's context ending fails the checks.
func checkCapabilityColumns(ctx context.Context, q querier, rel string,
	r *CheckResult) error {
	hasWAL, err := checkWALColumns(ctx, q, rel)
	if err := r.noteCapability(ctx, CapabilityWALColumns, err); err != nil {
		return err
	}
	r.HasWALColumns = hasWAL
	hasPlan, err := checkPlanTimeColumns(ctx, q, rel)
	if err := r.noteCapability(ctx, CapabilityPlanTimeColumns, err); err != nil {
		return err
	}
	r.HasPlanTimeColumns = hasPlan
	return nil
}

// noteCapability records a failed capability check, or returns it when the
// caller's context ended (shutdown, not a capability problem).
func (r *CheckResult) noteCapability(ctx context.Context, key string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s check: %w", key, err)
	}
	if r.CapabilityErrors == nil {
		r.CapabilityErrors = map[string]error{}
	}
	r.CapabilityErrors[key] = err
	return nil
}

// statementsRelation is pg_stat_statements qualified by its quoted schema.
func statementsRelation(schema string) string {
	if schema == "" {
		return "pg_stat_statements"
	}
	return pgx.Identifier{schema}.Sanitize() + ".pg_stat_statements"
}

// statementsColumnSQL reports whether the view has the column. A view is a
// relation, so pg_attribute lists its columns; to_regclass resolves the
// qualified name whatever the search_path.
const statementsColumnSQL = `/* pg_sage */ SELECT EXISTS(
	SELECT 1 FROM pg_catalog.pg_attribute
	 WHERE attrelid = pg_catalog.to_regclass($1)
	   AND attname = $2 AND attnum > 0 AND NOT attisdropped)`

// checkStatementsColumn reports whether rel has column. An error means
// unknown, not absent.
func checkStatementsColumn(ctx context.Context, q querier, rel, column string) (bool, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var exists bool
	if err := q.QueryRow(qctx, statementsColumnSQL, rel, column).Scan(&exists); err != nil {
		return false, fmt.Errorf("check %s for column %s: %w", rel, column, err)
	}
	return exists, nil
}

func checkPlanTimeColumns(ctx context.Context, q querier, rel string) (bool, error) {
	return checkStatementsColumn(ctx, q, rel, "total_plan_time")
}

func checkWALColumns(ctx context.Context, q querier, rel string) (bool, error) {
	return checkStatementsColumn(ctx, q, rel, "wal_records")
}
