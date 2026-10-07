package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The replace action's durable steps (sage.index_replace.state). Each step
// is recorded before it runs, so a restart knows what may have happened
// and reads the catalog to decide what to do next (resumeStepFor).
const (
	replaceCreating           = "creating"
	replaceCreated            = "created"
	replaceDropping           = "dropping"
	replaceCompleted          = "completed"
	replaceCreateFailed       = "create_failed"
	replaceDropFailed         = "drop_failed"
	replaceRollbackRecreating = "rollback_recreating"
	replaceRollbackDropping   = "rollback_dropping"
	replaceRolledBack         = "rolled_back"
	replaceRollbackFailed     = "rollback_failed"
	replaceOldRestoring       = "old_restoring"
	replaceOldRestored        = "old_restored"
)

// Verification phases: judging until the first verdict, then the soft-drop
// watch of the old index's users until the drop window ends.
const (
	replacePhaseNone    = "none"
	replacePhaseJudging = "judging"
	replacePhaseWatch   = "watch"
	replacePhaseDone    = "done"
)

// indexReplaceRecord is one row of sage.index_replace.
type indexReplaceRecord struct {
	ID, FindingID, ActionLogID, DecisionID int64
	ApprovedBy                             *int
	Plan                                   IndexReplace
	NewOID, OldOID                         int64
	OldDefinition, State, Phase, Err       string
	Before                                 map[string]any
}

// pairSQL and rollbackSQL are the statement pairs as approved.
func (r indexReplaceRecord) pairSQL() string {
	return r.Plan.CreateSQL + ";\n" + r.Plan.DropSQL + ";"
}

func (r indexReplaceRecord) rollbackSQL() string {
	return r.Plan.RecreateSQL + ";\n" + r.Plan.DropNewSQL + ";"
}

// replaceCatalog is what the catalog says about both indexes now.
type replaceCatalog struct {
	NewExists, NewValid, OldExists, OldValid bool
}

// replaceStep is what a resumed replacement does next.
type replaceStep string

const (
	stepNone        replaceStep = "none"
	stepFailCreate  replaceStep = "fail_create"
	stepDropRemnant replaceStep = "drop_remnant"
	stepDropOld     replaceStep = "drop_old"
	stepComplete    replaceStep = "complete"
	stepRestore     replaceStep = "restore"
	stepRecreateOld replaceStep = "recreate_old"
	stepDropNew     replaceStep = "drop_new"
	stepRolledBack  replaceStep = "rolled_back"
	stepRestoreOld  replaceStep = "restore_old"
	stepOldRestored replaceStep = "old_restored"
)

// resumeStepFor decides a resumed replacement's next step from its
// recorded state and the catalog. A build that never registered fails; an
// INVALID remnant is dropped first; a valid new index resumes the drop; a
// drop that committed completes; a new index that is gone or invalid after
// the build restores the original state; undo steps continue.
func resumeStepFor(state string, c replaceCatalog) replaceStep {
	switch state {
	case replaceCreating:
		switch {
		case !c.NewExists:
			return stepFailCreate
		case !c.NewValid:
			return stepDropRemnant
		}
		return stepDropOld
	case replaceCreated, replaceDropping:
		return forwardStep(c)
	case replaceRollbackRecreating:
		if !c.OldValid {
			return stepRecreateOld
		}
		return stepDropNew
	case replaceRollbackDropping:
		switch {
		case !c.OldValid:
			return stepRecreateOld
		case c.NewExists:
			return stepDropNew
		}
		return stepRolledBack
	case replaceOldRestoring:
		if c.OldValid {
			return stepOldRestored
		}
		return stepRestoreOld
	}
	return stepNone
}

// forwardStep: without a valid new index the original state is restored;
// with the old index gone (the drop committed, or someone dropped it) the
// replacement is complete; otherwise the drop runs (again).
func forwardStep(c replaceCatalog) replaceStep {
	switch {
	case !c.NewValid:
		return stepRestore
	case !c.OldExists:
		return stepComplete
	}
	return stepDropOld
}

// replaceGateView is what the gate sees of a statement pair: the build it
// validates and the objects it touches (the table and the old index).
func replaceGateView(sql string) (string, []string, bool) {
	create, drop, ok := optimizer.SplitIndexReplaceSQL(sql)
	if !ok {
		return "", nil, false
	}
	table := createIndexTable(normalizeSQLText(create))
	old := firstObjectAfter(normalizeSQLText(drop), "DROP INDEX", "CONCURRENTLY", "IF",
		"EXISTS")
	if table == "" || old == "" {
		return "", nil, false
	}
	return create, []string{table, old}, true
}

// replaceGateParts is the build and the drop of a statement pair.
func replaceGateParts(sql string) (string, string, bool) {
	return optimizer.SplitIndexReplaceSQL(sql)
}

const replaceColumns = `id, COALESCE(finding_id, 0), COALESCE(action_log_id, 0),
	COALESCE(decision_id, 0), approved_by, create_sql, drop_sql, rollback_sql,
	COALESCE(new_index_oid, 0), old_index_oid, old_definition, state, verify_phase,
	COALESCE(error, ''), before_state`

func scanReplace(row pgx.Row) (indexReplaceRecord, error) {
	var r indexReplaceRecord
	var create, drop, rollback string
	var before []byte
	err := row.Scan(&r.ID, &r.FindingID, &r.ActionLogID, &r.DecisionID, &r.ApprovedBy,
		&create, &drop, &rollback, &r.NewOID, &r.OldOID, &r.OldDefinition, &r.State,
		&r.Phase, &r.Err, &before)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(before, &r.Before); err != nil {
		return r, fmt.Errorf("decode replace %d before_state: %w", r.ID, err)
	}
	r.Plan, err = ParseIndexReplace(create+";\n"+drop+";", rollback)
	if err != nil {
		return r, fmt.Errorf("replace %d: %w", r.ID, err)
	}
	return r, nil
}

// insertReplace records a replacement about to build its index.
func (e *Executor) insertReplace(ctx context.Context, r *indexReplaceRecord) error {
	before, err := json.Marshal(r.Before)
	if err != nil {
		return fmt.Errorf("encode replace before_state: %w", err)
	}
	return e.pool.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.index_replace
		(database_id, finding_id, decision_id, approved_by, table_name, new_index,
		 create_sql, drop_sql, old_index, old_index_oid, old_definition, rollback_sql,
		 state, before_state)
		VALUES ($1, NULLIF($2, 0), NULLIF($3, 0), $4, $5, $6, $7, $8, $9, $10, $11, $12,
		        $13, $14) RETURNING id`,
		e.databaseIDValue(), r.FindingID, r.DecisionID, r.ApprovedBy, r.Plan.Table,
		r.Plan.NewIndex, r.Plan.CreateSQL, r.Plan.DropSQL, r.Plan.OldIndex, r.OldOID,
		r.OldDefinition, r.rollbackSQL(), r.State, before).Scan(&r.ID)
}

var errReplaceStateMoved = errors.New("index replace: another worker moved the replacement")

// moveReplace sets the state when it is still one of from (compare and
// set), with the error text when not empty.
func (e *Executor) moveReplace(ctx context.Context, r *indexReplaceRecord, to, errText string,
	from ...string) error {
	tag, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.index_replace
		SET state = $2, error = COALESCE(NULLIF($3, ''), error), updated_at = now()
		WHERE id = $1 AND state = ANY($4)`, r.ID, to, errText, from)
	if err != nil {
		return fmt.Errorf("record replace step %s: %w", to, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w (to %s)", errReplaceStateMoved, to)
	}
	r.State = to
	if errText != "" {
		r.Err = errText
	}
	return nil
}

// setReplaceFields records the new index's OID, the action and the phase.
func (e *Executor) setReplaceFields(ctx context.Context, r indexReplaceRecord) error {
	_, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.index_replace
		SET new_index_oid = NULLIF($2, 0), action_log_id = NULLIF($3, 0),
		    verify_phase = $4, updated_at = now() WHERE id = $1`,
		r.ID, r.NewOID, r.ActionLogID, r.Phase)
	if err != nil {
		return fmt.Errorf("record replace %d: %w", r.ID, err)
	}
	return nil
}

func (e *Executor) loadReplaceByAction(ctx context.Context, actionID int64) (
	indexReplaceRecord, error) {
	return scanReplace(e.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+replaceColumns+`
		FROM sage.index_replace WHERE action_log_id = $1 ORDER BY id DESC LIMIT 1`,
		actionID))
}

// loadOpenReplaces reads the replacements with a step or a watch to resume.
func (e *Executor) loadOpenReplaces(ctx context.Context) ([]indexReplaceRecord, error) {
	rows, err := e.pool.Query(ctx, `/* pg_sage */ SELECT `+replaceColumns+`
		FROM sage.index_replace
		WHERE state IN ('creating', 'created', 'dropping', 'rollback_recreating',
		      'rollback_dropping', 'old_restoring')
		   OR verify_phase IN ('judging', 'watch')
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load open replacements: %w", err)
	}
	defer rows.Close()
	var out []indexReplaceRecord
	for rows.Next() {
		r, err := scanReplace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open replacements: %w", err)
	}
	return out, nil
}
