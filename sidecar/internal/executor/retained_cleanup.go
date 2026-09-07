package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

var errSupersededIndexGone = errors.New("superseded index already removed")

var ErrReviewedCleanupRequired = errors.New(
	"reviewed_cleanup_required: retained new index; superseded index preserved because " +
		"DROP INDEX CONCURRENTLY cannot bind to the verified object identity",
)

var supersededDropPattern = regexp.MustCompile(
	`(?i)^\s*DROP\s+INDEX\s+CONCURRENTLY\s+(?:IF\s+EXISTS\s+)?(` +
		migrationIdentifier + `(?:\s*\.\s*` + migrationIdentifier + `)?)` +
		`\s*(?:RESTRICT\s*)?;?\s*$`,
)

// Preserve immutable cleanup intent with the CREATE action so resumed watches do
// not depend on a later, edited finding. The OID detects a replaced old index.
func (e *Executor) snapshotSupersededIndex(
	ctx context.Context, f analyzer.Finding, state map[string]any,
) error {
	drop, _ := f.Detail["drop_ddl"].(string)
	if drop == "" || isSelfReferentialDrop(f.RecommendedSQL, drop) {
		return nil
	}
	target, err := supersededIndexTarget(drop)
	if err != nil {
		return err
	}
	var oid int64
	err = e.pool.QueryRow(ctx, `SELECT COALESCE(to_regclass($1)::bigint,0)`, target).Scan(&oid)
	if err != nil {
		return fmt.Errorf("snapshot superseded index: %w", err)
	}
	if oid == 0 {
		return errors.New("superseded index no longer exists")
	}
	state["superseded_index_drop_sql"] = drop
	state["superseded_index_oid"] = oid
	return nil
}

func validateSupersededDrop(sql string) error {
	_, err := supersededIndexTarget(sql)
	return err
}

func supersededIndexTarget(sql string) (string, error) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return "", err
	}
	match := supersededDropPattern.FindStringSubmatch(sql)
	if len(match) != 2 {
		return "", errors.New("superseded cleanup requires one DROP INDEX CONCURRENTLY target")
	}
	return match[1], nil
}

func (e *Executor) cleanupRetainedIndex(ctx context.Context, actionID int64) error {
	e.retainedCleanupMu.Lock()
	defer e.retainedCleanupMu.Unlock()
	var createSQL string
	var data []byte
	var reviewed bool
	err := e.pool.QueryRow(ctx, `SELECT sql_executed,COALESCE(before_state,'{}'::jsonb),
		EXISTS(SELECT 1 FROM sage.verification WHERE action_log_id=$1 AND reason=$2)
		FROM sage.action_log WHERE id=$1 AND action_type='create_index'`,
		actionID, ErrReviewedCleanupRequired.Error()).
		Scan(&createSQL, &data, &reviewed)
	if err != nil {
		return fmt.Errorf("load retained index cleanup: %w", err)
	}
	var state struct {
		DropSQL string `json:"superseded_index_drop_sql"`
		OID     int64  `json:"superseded_index_oid"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode cleanup intent: %w", err)
	}
	if state.DropSQL == "" || isSelfReferentialDrop(createSQL, state.DropSQL) {
		return nil
	}
	if reviewed {
		return ErrReviewedCleanupRequired
	}
	if err := validateSupersededDrop(state.DropSQL); err != nil {
		return err
	}
	if err := e.validateRetainedReplacement(ctx, createSQL, state.DropSQL, state.OID); err != nil {
		if errors.Is(err, errSupersededIndexGone) {
			return nil
		}
		return err
	}
	return e.reviewSupersededIndexCleanup(ctx, actionID, state.DropSQL)
}

func (e *Executor) validateRetainedReplacement(
	ctx context.Context, createSQL, dropSQL string, oid int64,
) error {
	target, err := supersededIndexTarget(dropSQL)
	if err != nil {
		return err
	}
	var current int64
	err = e.pool.QueryRow(ctx, `SELECT COALESCE(to_regclass($1)::bigint,0)`, target).Scan(&current)
	if err != nil {
		return fmt.Errorf("lookup superseded index: %w", err)
	}
	if current == 0 {
		return errSupersededIndexGone
	}
	if oid <= 0 || current != oid {
		return errors.New("superseded index identity changed; preserving current index")
	}
	return e.checkRetainedCoverage(ctx, createSQL, current)
}

func (e *Executor) reviewSupersededIndexCleanup(
	ctx context.Context, actionID int64, sql string,
) error {
	target, err := supersededIndexTarget(sql)
	if err != nil {
		return err
	}
	if e.checkEmergencyStop(ctx) {
		return errors.New("emergency stop withheld superseded cleanup")
	}
	candidate := analyzer.Finding{RecommendedSQL: sql, ObjectType: "index",
		ObjectIdentifier: target, Title: "Retained index superseded cleanup"}
	decision := e.evaluateFindingPolicy(ctx, candidate, false)
	if decision.Decision != PolicyDecisionExecute {
		return errors.New("standing policy withheld superseded-index cleanup")
	}
	// Neither a process mutex nor a cooperative lease protects against external
	// DDL replacing this name. Preserve both indexes instead of a name-based DROP.
	result, err := e.pool.Exec(ctx, `UPDATE sage.verification SET reason=$2,updated_at=now()
		WHERE action_log_id=$1 AND verdict='success'`, actionID, ErrReviewedCleanupRequired.Error())
	if err != nil {
		return fmt.Errorf("record superseded cleanup review requirement: %w", err)
	}
	if result.RowsAffected() == 0 {
		return errors.New("retained verification missing; cleanup review requirement was not recorded")
	}
	return ErrReviewedCleanupRequired
}

// Only an equivalent key definition with at least the old included columns can
// supersede an ordinary index. Unique, constraint and partial semantics survive.
func (e *Executor) checkRetainedCoverage(ctx context.Context, sql string, oldOID int64) error {
	schema, table, _, ok := parseCreateIndexTarget(sql)
	if !ok {
		return errors.New("cannot resolve retained index table")
	}
	target := pgx.Identifier{table}
	if schema != "" {
		target = pgx.Identifier{schema, table}
	}
	parts := splitQualifiedIdentifier(extractIndexName(sql))
	if len(parts) == 0 {
		return errors.New("cannot resolve retained index name")
	}
	var covered bool
	err := e.pool.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_index n JOIN pg_class nc ON nc.oid=n.indexrelid
 JOIN pg_index o ON o.indexrelid=$1 JOIN pg_class oc ON oc.oid=o.indexrelid
 WHERE n.indrelid=to_regclass($2) AND nc.relname=$3 AND n.indrelid=o.indrelid
 AND n.indisvalid AND n.indisready AND NOT o.indisunique AND NOT o.indisprimary
 AND NOT o.indisexclusion AND n.indnkeyatts=o.indnkeyatts AND nc.relam=oc.relam
 AND n.indclass=o.indclass AND n.indcollation=o.indcollation AND n.indoption=o.indoption
 AND (n.indkey::smallint[])[0:n.indnkeyatts-1]=(o.indkey::smallint[])[0:o.indnkeyatts-1]
 AND o.indkey::smallint[] <@ n.indkey::smallint[]
 AND COALESCE(pg_get_expr(n.indpred,n.indrelid),'')=COALESCE(pg_get_expr(o.indpred,o.indrelid),'')
 AND COALESCE(pg_get_expr(n.indexprs,n.indrelid),'')=COALESCE(pg_get_expr(o.indexprs,o.indrelid),'')
 )`, oldOID, target.Sanitize(), parts[len(parts)-1]).Scan(&covered)
	if err != nil {
		return fmt.Errorf("check retained index coverage: %w", err)
	}
	if !covered {
		return errors.New("retained index does not safely cover superseded index")
	}
	return nil
}
