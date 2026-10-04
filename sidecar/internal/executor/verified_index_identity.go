package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// verifiedActionForFinding admits only a plain CREATE INDEX CONCURRENTLY
// whose rollback drops exactly the index it creates. IF NOT EXISTS can turn
// the create into a no-op on a user's index; UNIQUE is a schema decision,
// not a performance one; a rollback naming another index would drop it.
func verifiedActionForFinding(f analyzer.Finding) (verifiedIndexAction, error) {
	queryIDs := targetQueryIDs(f)
	indexName := createIndexIdentifier(f.RecommendedSQL)
	// Optimizer identities are "schema.table|<index definition>" (C05).
	table := strings.TrimSpace(analyzer.OptimizerFindingTable(f))
	if err := verifiablePreconditions(indexName, table, f.RollbackSQL, queryIDs); err != nil {
		return verifiedIndexAction{}, err
	}
	upper := strings.ToUpper(normalizeSQLText(f.RecommendedSQL))
	if strings.Contains(upper, " IF NOT EXISTS ") ||
		strings.HasPrefix(upper, "CREATE UNIQUE ") {
		return verifiedIndexAction{}, fmt.Errorf(
			"%w: verified create must be a plain CREATE INDEX CONCURRENTLY",
			ErrVerificationUnavailable)
	}
	if err := rollbackTargetsCreatedIndex(f.RecommendedSQL, f.RollbackSQL); err != nil {
		return verifiedIndexAction{}, err
	}
	return verifiedIndexAction{
		SQL: f.RecommendedSQL, RollbackSQL: f.RollbackSQL,
		Table: table, IndexName: indexName, QueryIDs: queryIDs,
		Criterion: verify.Criterion{Kind: "per_query_latency"},
	}, nil
}

// rollbackTargetsCreatedIndex checks that the finding's rollback identifies
// the created index. It is never executed: prepareVerifiedIndex re-derives
// the rollback schema-qualified, so the identifying DROP may be unqualified
// (the executed-SQL rule for destructive DDL applies to what actually runs).
func rollbackTargetsCreatedIndex(createSQL, rollbackSQL string) error {
	match := supersededDropPattern.FindStringSubmatch(rollbackSQL)
	if len(match) != 2 {
		return fmt.Errorf("%w: rollback must be one DROP INDEX CONCURRENTLY",
			ErrVerificationUnavailable)
	}
	target := match[1]
	parts := splitQualifiedIdentifier(target)
	name := parts[len(parts)-1]
	if !strings.HasPrefix(strings.TrimSpace(target), `"`) && len(parts) == 1 {
		name = strings.ToLower(name)
	}
	if !strings.EqualFold(name, createIndexIdentifier(createSQL)) {
		return fmt.Errorf("%w: rollback drops %s, not the created index",
			ErrVerificationUnavailable, target)
	}
	createSchema, _, _, _ := parseCreateIndexTarget(createSQL)
	if len(parts) == 2 && createSchema != "" && !strings.EqualFold(parts[0], createSchema) {
		return fmt.Errorf("%w: rollback targets schema %s, index lives in %s",
			ErrVerificationUnavailable, parts[0], createSchema)
	}
	return nil
}

// prepareVerifiedIndex binds the action to a schema-qualified identity:
// the index name must not exist yet (so the create cannot be a no-op on a
// pre-existing index), and the rollback is derived deterministically as a
// drop of that qualified name instead of trusting LLM-authored SQL.
func (e *Executor) prepareVerifiedIndex(ctx context.Context, action *verifiedIndexAction) error {
	schemaName, tableName, _, ok := parseCreateIndexTarget(action.SQL)
	if !ok {
		return fmt.Errorf("%w: cannot parse index target", ErrVerificationUnavailable)
	}
	tableRef := pgx.Identifier{tableName}
	if schemaName != "" {
		tableRef = pgx.Identifier{schemaName, tableName}
	}
	var resolved string
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT n.nspname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, tableRef.Sanitize()).Scan(&resolved)
	if err != nil {
		return fmt.Errorf("%w: resolve index table: %v", ErrVerificationUnavailable, err)
	}
	qualified := pgx.Identifier{resolved, action.IndexName}.Sanitize()
	var exists bool
	if err := e.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", qualified).
		Scan(&exists); err != nil {
		return fmt.Errorf("check index name %s: %w", qualified, err)
	}
	if exists {
		return fmt.Errorf("%w: index %s already exists", ErrVerificationUnavailable, qualified)
	}
	action.IndexName = qualified
	action.RollbackSQL = "DROP INDEX CONCURRENTLY IF EXISTS " + qualified
	return nil
}

// recordCreatedIndexIdentity stores the OID of the index this action built,
// so a later revert can prove it drops that same object.
func (e *Executor) recordCreatedIndexIdentity(
	ctx context.Context, qualified string, state map[string]any,
) {
	var oid int64
	err := e.pool.QueryRow(ctx, "SELECT COALESCE(to_regclass($1)::oid::bigint, 0)",
		qualified).Scan(&oid)
	if err != nil || oid == 0 {
		e.logFn("executor", "created index %s identity unavailable: %v", qualified, err)
		return
	}
	state["created_index"] = qualified
	state["created_index_oid"] = oid
}

type createdIndexIdentity struct {
	Name string `json:"created_index"`
	OID  int64  `json:"created_index_oid"`
}

func (e *Executor) loadCreatedIndexIdentity(
	ctx context.Context, actionID int64,
) (createdIndexIdentity, error) {
	var raw []byte
	var identity createdIndexIdentity
	err := e.pool.QueryRow(ctx, `SELECT COALESCE(before_state, '{}'::jsonb)
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&raw)
	if err != nil {
		return identity, fmt.Errorf("load created index identity: %w", err)
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return identity, fmt.Errorf("decode created index identity: %w", err)
	}
	if identity.Name == "" || identity.OID <= 0 {
		return identity, errors.New("created index identity was not recorded")
	}
	return identity, nil
}

// currentIndexOID returns the OID the name resolves to now (0 when gone).
func (e *Executor) currentIndexOID(ctx context.Context, qualified string) (int64, error) {
	var oid int64
	err := e.pool.QueryRow(ctx, "SELECT COALESCE(to_regclass($1)::oid::bigint, 0)",
		qualified).Scan(&oid)
	return oid, err
}

// authorizeCreatedIndexRevert asks the standing gate to authorize dropping
// an index this executor created (identity already verified), using the
// revert contract rather than the approval-gated unused-index drop.
func (e *Executor) authorizeCreatedIndexRevert(ctx context.Context, dropSQL, target string) bool {
	contract := revertCreatedIndexContract()
	gate := e.StandingPolicyGate()
	if gate == nil {
		return false // the standing gate is the only authority (G4-I01)
	}
	decision := standingPolicyDecision(gate.Authorize(ctx, policy.ActionRequest{
		SQL: dropSQL, Feature: string(policy.ChangeIndex), TargetObjs: []string{target},
		Contract: policyContract(contract),
	}))
	// A revert restores the state before a verified build; like a rollback,
	// its authorization holds no budget slot.
	e.releaseBudget(ctx, decision.DecisionID)
	return decision.Decision == PolicyDecisionExecute
}

// completeVerificationRevert marks the durable verification complete once
// its revert effect has succeeded (or can never be performed).
func (e *Executor) completeVerificationRevert(ctx context.Context, actionID int64) {
	if _, err := e.pool.Exec(ctx, `UPDATE sage.verification
		SET completed_at = now(), updated_at = now()
		WHERE action_log_id = $1 AND completed_at IS NULL`, actionID); err != nil {
		e.logFn("executor", "complete verification for action %d: %v", actionID, err)
	}
}

// admitVerifiedCreate validates, admits and identity-binds an autonomous
// CREATE INDEX, replacing the finding's rollback with the deterministic
// drop of the qualified index it will create.
func (e *Executor) admitVerifiedCreate(
	ctx context.Context, f *analyzer.Finding, beforeState map[string]any,
	findingID, decisionID int64,
) (verifiedIndexAction, error) {
	action, err := verifiedActionForFinding(*f)
	if err != nil {
		return action, err
	}
	if e.indexVerification == nil {
		return action, ErrVerificationUnavailable
	}
	key := admissionFindingKey(findingID, f.RecommendedSQL)
	if err := e.admitIndexBuild(ctx, key, decisionID); err != nil {
		return action, err
	}
	if err := e.prepareVerifiedIndex(ctx, &action); err != nil {
		return action, err
	}
	f.RollbackSQL = action.RollbackSQL
	return action, e.snapshotSupersededIndex(ctx, *f, beforeState)
}

// verifiablePreconditions names the first missing piece a verified index
// build needs (dogfood lifeos-1: a bare "index verification unavailable"
// read as if verification were not wired).
func verifiablePreconditions(indexName, table, rollbackSQL string, queryIDs []int64) error {
	switch {
	case indexName == "":
		return fmt.Errorf("%w: the proposed CREATE INDEX has no index name",
			ErrVerificationUnavailable)
	case table == "":
		return fmt.Errorf("%w: the proposal names no target table", ErrVerificationUnavailable)
	case rollbackSQL == "":
		return fmt.Errorf("%w: the proposal has no rollback", ErrVerificationUnavailable)
	case len(queryIDs) == 0:
		return fmt.Errorf("%w: no target queries to verify against (no workload on "+
			"the table yet)", ErrVerificationUnavailable)
	}
	return nil
}
