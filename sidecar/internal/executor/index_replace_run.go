package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/verify"
)

// replaceHooks are test seams around the step boundaries: an error returned
// stops the run where it is, as a crash would (nothing is recorded or
// undone).
type replaceHooks struct {
	afterCreate func(context.Context) error
	beforeDrop  func(context.Context) error
}

func runHook(ctx context.Context, hook func(context.Context) error) error {
	if hook == nil {
		return nil
	}
	return hook(ctx)
}

// ExecuteIndexReplace runs an operator-approved replacement: sql is the
// statement pair (build, drop), rollbackSQL its undo, findingID the open
// finding that proposed exactly that pair. The old index's recorded OID
// and definition must still hold. It returns the action_log id; a failed
// build returns ErrReplaceCreateFailed (nothing dropped), a failed drop
// ErrReplacePartial (the new index stays) with the recorded action.
func (e *Executor) ExecuteIndexReplace(ctx context.Context, findingID int, sql,
	rollbackSQL string, approvedBy *int) (int64, error) {
	plan, err := ParseIndexReplace(sql, rollbackSQL)
	if err != nil {
		return 0, err
	}
	release, err := e.acquireDDLSlot(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	runCtx, cancel := e.detachedDDLContext(ctx)
	defer cancel()
	if preview := e.explainOperatorAction(runCtx, sql); preview.Decision !=
		PolicyDecisionExecute {
		return 0, fmt.Errorf("policy refused operator action: %s",
			humanPolicyReason(preview))
	}
	finding, err := e.verifyManualFinding(runCtx, findingID, sql)
	if err != nil {
		return 0, err
	}
	rec, err := e.prepareReplace(runCtx, plan, detailMap(finding.detail))
	if err != nil {
		return 0, err
	}
	rec.FindingID, rec.ApprovedBy = int64(findingID), approvedBy
	run := &replaceRun{e: e, rec: rec, sql: sql}
	return e.Apply(runCtx, ActionIntent{
		Authorize: func(ctx context.Context) (ActionPolicyDecision, error) {
			decision, err := e.authorizeOperatorAction(ctx, sql, findingID, approvedBy)
			return standingPolicyDecision(decision), err
		},
		TargetLease: operatorLease(sql, approvedBy),
		SlotHeld:    true, Execute: run.execute, Verify: run.verify,
	})
}

// prepareReplace checks the old index against the catalog and records what
// the verification needs: the old definition (the soft drop), the targeted
// queries, the old index's users and their frozen baseline.
func (e *Executor) prepareReplace(ctx context.Context, plan IndexReplace,
	detail map[string]any) (indexReplaceRecord, error) {
	recorded, _ := detail["index_replace"].(map[string]any)
	oid := detailInt64(recorded["old_index_oid"])
	def, _ := recorded["old_definition"].(string)
	old, err := e.checkOldIndex(ctx, plan, oid, def)
	if err != nil {
		return indexReplaceRecord{}, err
	}
	if err := e.checkForeignKeys(ctx, plan, old); err != nil {
		return indexReplaceRecord{}, err
	}
	rec := indexReplaceRecord{Plan: plan, OldOID: old.OID, OldDefinition: old.Definition,
		State: replaceCreating, Phase: replacePhaseNone}
	rec.Before = e.replaceBeforeState(ctx, plan, old, detail)
	return rec, nil
}

// replaceBeforeState is the action's before_state: the old index (kept for
// the soft drop), the prediction, the targeted queries, the old index's
// users (guarded) and one frozen baseline for both.
func (e *Executor) replaceBeforeState(ctx context.Context, plan IndexReplace,
	old oldIndexInfo, detail map[string]any) map[string]any {
	targets := targetQueryIDs(analyzer.Finding{Detail: detail})
	before := e.snapshotBeforeState(ctx, targets)
	before["replaced_index"] = plan.OldIndex
	before["replaced_index_oid"] = old.OID
	before["replaced_index_definition"] = old.Definition
	p := predictionFromDetail(verify.ClassIndexReplace, detail)
	p.Class, p.TargetQueryIDs = verify.ClassIndexReplace, targets
	before["predicted_effect"] = p
	var guarded []int64
	for _, id := range e.dropTargets(ctx, plan.DropSQL, before) {
		if !containsID(targets, id) {
			guarded = append(guarded, id)
		}
	}
	before["guarded_queryids"] = guarded
	if all := append(append([]int64(nil), targets...), guarded...); len(all) > 0 {
		before["verify_baseline"] = e.freezeBaseline(ctx, verify.ClassIndexCreate, all)
	}
	return before
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// replaceRun carries one replacement through Apply's Execute and Verify.
type replaceRun struct {
	e        *Executor
	rec      indexReplaceRecord
	sql      string
	actionID int64
}

// execute claims the recommendation, runs both steps and settles the claim.
func (r *replaceRun) execute(ctx context.Context, decision ActionPolicyDecision) (
	int64, error) {
	e := r.e
	claim, err := e.claimForOperator(ctx, int(r.rec.FindingID), r.sql, r.rec.ApprovedBy)
	if err != nil {
		return 0, err
	}
	r.rec.DecisionID = decision.DecisionID
	actionID, err := r.steps(ctx, decision, claim)
	e.settleClaim(ctx, claim, actionID, err)
	r.actionID = actionID
	return actionID, err
}

// steps records the replacement, builds the new index, checks it, then
// drops the old one. Each step is recorded before it runs.
func (r *replaceRun) steps(ctx context.Context, decision ActionPolicyDecision,
	claim *recommendation.Claim) (int64, error) {
	e, rec := r.e, &r.rec
	if err := e.insertReplace(ctx, rec); err != nil {
		return 0, err
	}
	if err := e.replaceBuild(ctx, rec, decision); err != nil {
		id := e.logReplaceAction(ctx, *rec, err, claim)
		return id, err
	}
	if err := runHook(ctx, e.replaceHooks.afterCreate); err != nil {
		return 0, err
	}
	return e.replaceDropStep(ctx, rec, decision, claim)
}

// replaceBuild runs the CREATE INDEX CONCURRENTLY and checks the result;
// on failure it drops an INVALID remnant and records create_failed.
func (e *Executor) replaceBuild(ctx context.Context, rec *indexReplaceRecord,
	decision ActionPolicyDecision) error {
	sql := rec.Plan.CreateSQL
	buildErr := ExecConcurrently(ctx, e.pool, sql, e.ddlTimeout(), e.lockOption(sql, decision))
	if buildErr == nil {
		oid, err := e.validNewIndex(ctx, rec.Plan)
		rec.NewOID, buildErr = oid, err
	}
	if buildErr == nil {
		if err := e.moveReplace(ctx, rec, replaceCreated, "", replaceCreating); err != nil {
			return err
		}
		return e.setReplaceFields(ctx, *rec)
	}
	if err := e.dropInvalidIndex(ctx, rec.Plan.NewIndex); err != nil {
		buildErr = errors.Join(buildErr, err)
	}
	if err := e.moveReplace(ctx, rec, replaceCreateFailed, buildErr.Error(),
		replaceCreating); err != nil {
		buildErr = errors.Join(buildErr, err)
	}
	return fmt.Errorf("%w: %w", ErrReplaceCreateFailed, buildErr)
}

// validNewIndex is the new index's OID once it is valid and ready.
func (e *Executor) validNewIndex(ctx context.Context, p IndexReplace) (int64, error) {
	c, err := e.replaceCatalogView(ctx, p)
	if err != nil {
		return 0, err
	}
	if !c.NewValid {
		return 0, fmt.Errorf("%s is not valid and ready after its build", p.NewIndex)
	}
	return e.indexOID(ctx, p.NewIndex)
}

// dropInvalidIndex drops index only while it is INVALID (a failed build's
// remnant); a valid index of that name is never touched.
func (e *Executor) dropInvalidIndex(ctx context.Context, index string) error {
	var invalid bool
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass($1) AND NOT indisvalid)`, index).Scan(&invalid)
	if err != nil || !invalid {
		return err
	}
	drop := "DROP INDEX CONCURRENTLY IF EXISTS " + index
	return ExecConcurrently(ctx, e.pool, drop, e.ddlTimeout(),
		WithLockTimeout(e.lockTimeoutMS(drop, ActionPolicyDecision{})))
}

// replaceDropStep re-checks the old index's identity, then drops it. A
// failed drop is a partial result: the new index stays, the old is kept.
func (e *Executor) replaceDropStep(ctx context.Context, rec *indexReplaceRecord,
	decision ActionPolicyDecision, claim *recommendation.Claim) (int64, error) {
	if _, err := e.checkOldIndex(ctx, rec.Plan, rec.OldOID, rec.OldDefinition); err != nil {
		return e.abandonBuild(ctx, rec, err, claim)
	}
	if err := runHook(ctx, e.replaceHooks.beforeDrop); err != nil {
		return 0, err
	}
	if err := e.moveReplace(ctx, rec, replaceDropping, "", replaceCreated,
		replaceDropping); err != nil {
		return 0, err
	}
	sql := rec.Plan.DropSQL
	if err := ExecConcurrently(ctx, e.pool, sql, e.ddlTimeout(),
		e.lockOption(sql, decision)); err != nil {
		return e.partialReplace(ctx, rec, err, claim)
	}
	return e.completeReplace(ctx, rec, claim)
}

// abandonBuild undoes the build when the old index changed before the
// drop: nothing of the replacement remains.
func (e *Executor) abandonBuild(ctx context.Context, rec *indexReplaceRecord, cause error,
	claim *recommendation.Claim) (int64, error) {
	err := e.dropNewIndex(ctx, *rec)
	if err == nil {
		err = e.moveReplace(ctx, rec, replaceRolledBack, cause.Error(), replaceCreated,
			replaceDropping)
	}
	id := e.logReplaceAction(ctx, *rec, cause, claim)
	return id, errors.Join(cause, err)
}

func (e *Executor) partialReplace(ctx context.Context, rec *indexReplaceRecord,
	dropErr error, claim *recommendation.Claim) (int64, error) {
	cause := fmt.Errorf("%w: %w", ErrReplacePartial, dropErr)
	stateErr := e.moveReplace(ctx, rec, replaceDropFailed, dropErr.Error(), replaceDropping)
	id := e.logReplaceAction(ctx, *rec, cause, claim)
	if id > 0 {
		updateActionOutcome(ctx, e.pool, id, "partial", cause.Error())
	}
	rec.ActionLogID = id
	if err := e.setReplaceFields(ctx, *rec); err != nil {
		stateErr = errors.Join(stateErr, err)
	}
	return id, errors.Join(cause, stateErr)
}

// completeReplace records the finished replacement and starts judging it.
func (e *Executor) completeReplace(ctx context.Context, rec *indexReplaceRecord,
	claim *recommendation.Claim) (int64, error) {
	if err := e.moveReplace(ctx, rec, replaceCompleted, "", replaceDropping, replaceCreated,
		replaceCreating); err != nil {
		return 0, err
	}
	id := e.logReplaceAction(ctx, *rec, nil, claim)
	if id <= 0 {
		return 0, errors.New("index replace: the completed replacement was not recorded")
	}
	e.recordPrediction(ctx, id, rec.Before)
	rec.ActionLogID, rec.Phase = id, replacePhaseJudging
	return id, e.setReplaceFields(ctx, *rec)
}

// logReplaceAction records the replacement in sage.action_log (failed when
// execErr is set, monitoring otherwise).
func (e *Executor) logReplaceAction(ctx context.Context, rec indexReplaceRecord,
	execErr error, claim *recommendation.Claim) int64 {
	return e.logManualActionWithDecision(ctx, int(rec.FindingID), rec.pairSQL(),
		rec.rollbackSQL(), rec.Before, execErr, rec.ApprovedBy, rec.DecisionID, claim)
}

// verify starts the replacement's verification watch.
func (r *replaceRun) verify(ctx context.Context, actionID int64) error {
	if actionID <= 0 || r.rec.State != replaceCompleted {
		return nil
	}
	r.e.notifyPostDDL(ctx, r.rec.Plan.CreateSQL)
	r.e.watchIndexReplace(ctx, actionID)
	return nil
}

// isReplaceAction reports whether an action_log row is a replacement.
func (e *Executor) isReplaceAction(ctx context.Context, actionID int64) bool {
	var actionType string
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT action_type FROM sage.action_log
		WHERE id = $1`, actionID).Scan(&actionType)
	return err == nil && strings.EqualFold(actionType, ActionTypeReplaceIndex)
}
