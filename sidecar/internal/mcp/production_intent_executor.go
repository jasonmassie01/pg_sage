package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
)

type ProductionIntentExecutor struct {
	store            IntentStore
	planner          *plan.Planner
	gate             policy.Gate
	migrationRuntime MigrationRuntime
}

func (executor *ProductionIntentExecutor) WithMigrationRuntime(
	runtime MigrationRuntime,
) *ProductionIntentExecutor {
	if executor != nil {
		executor.migrationRuntime = runtime
	}
	return executor
}

func NewProductionIntentExecutor(
	store IntentStore, planner *plan.Planner, gates ...policy.Gate,
) *ProductionIntentExecutor {
	executor := &ProductionIntentExecutor{store: store, planner: planner}
	if len(gates) > 0 && gates[0] != nil {
		executor.gate = NeverApproved(gates[0])
	}
	return executor
}

func (executor *ProductionIntentExecutor) ExecuteConcrete(
	ctx context.Context, request policy.ActionRequest,
) (any, error) {
	if executor == nil || executor.gate == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	if request.Contract == nil {
		return nil, errors.New("concrete intent requires a typed contract")
	}
	switch request.Contract.ActionType {
	case "optimize_query", "ensure_fk_indexes":
		return executor.authorizeCandidates(ctx, request)
	case "apply_migration":
		return executor.authorizeMigration(ctx, request)
	default:
		return nil, fmt.Errorf("unsupported concrete intent %q", request.Contract.ActionType)
	}
}

func (executor *ProductionIntentExecutor) Execute(
	ctx context.Context, request policy.ActionRequest, decision policy.Decision,
) (any, error) {
	if executor == nil || executor.store == nil || executor.planner == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	if request.Contract == nil || decision.Verdict != policy.VerdictExecute {
		return nil, errors.New("intent execution requires an authorized typed contract")
	}
	switch request.Contract.ActionType {
	case "declare_table_contract":
		return executor.declareTableContract(ctx, request, decision)
	case "register_consumer":
		return executor.registerConsumer(ctx, request)
	case "apply_migration":
		return executor.planMigration(ctx, request, decision)
	case "optimize_query", "ensure_fk_indexes":
		return executor.findCandidates(ctx, request)
	default:
		return nil, fmt.Errorf("unsupported executable intent %q", request.Contract.ActionType)
	}
}

func (executor *ProductionIntentExecutor) declareTableContract(
	ctx context.Context, request policy.ActionRequest, decision policy.Decision,
) (WriteOutcome, error) {
	var input struct {
		Table      string          `json:"table"`
		AppendOnly bool            `json:"append_only"`
		Retention  json.RawMessage `json:"retention"`
		ExpectedPK string          `json:"expected_pk"`
		Exemptions json.RawMessage `json:"exemptions"`
	}
	if err := json.Unmarshal(request.Arguments, &input); err != nil {
		return WriteOutcome{}, fmt.Errorf("%w: decode table contract: %v", ErrIntentInvalid,
			err)
	}
	schemaName, tableName, err := qualifiedName(input.Table)
	if err != nil {
		return WriteOutcome{}, err
	}
	retention, err := parseRetention(input.Retention)
	if err != nil {
		return WriteOutcome{}, err
	}
	return executor.store.DeclareTableContract(ctx, TableContractDeclaration{
		DatabaseID: request.DatabaseID, Schema: schemaName, Table: tableName,
		AppendOnly: input.AppendOnly, Retention: retention.Interval,
		RetentionColumn: retention.Column,
		ExpectedPK:      strings.TrimSpace(input.ExpectedPK), Exemptions: input.Exemptions,
		DeclaredBy: ActorFromContext(ctx), EvidenceID: decision.EvidenceID,
	})
}

func (executor *ProductionIntentExecutor) registerConsumer(
	ctx context.Context, request policy.ActionRequest,
) (WriteOutcome, error) {
	var input struct {
		SlotName string `json:"slot_name"`
		Owner    string `json:"owner"`
		Identity string `json:"consumer_identity"`
	}
	if err := json.Unmarshal(request.Arguments, &input); err != nil {
		return WriteOutcome{}, fmt.Errorf("%w: decode consumer registration: %v",
			ErrIntentInvalid, err)
	}
	if strings.TrimSpace(input.SlotName) == "" || strings.TrimSpace(input.Owner) == "" {
		return WriteOutcome{}, fmt.Errorf("%w: slot_name and owner are required",
			ErrIntentInvalid)
	}
	return executor.store.RegisterConsumer(ctx, ConsumerRegistration{
		SlotName: strings.TrimSpace(input.SlotName), Owner: strings.TrimSpace(input.Owner),
		ConsumerIdentity: strings.TrimSpace(input.Identity),
	})
}

// planMigration returns the safe plan for an apply_migration the gate
// authorized and records it as recommend_only; it never runs the DDL.
func (executor *ProductionIntentExecutor) planMigration(
	ctx context.Context, request policy.ActionRequest, decision policy.Decision,
) (MigrationOutcome, error) {
	input, planned, err := executor.buildMigrationPlan(ctx, request)
	if err != nil {
		return MigrationOutcome{}, err
	}
	return executor.recordMigrationPlan(ctx, request, input, MigrationOutcome{
		Verdict: "recommend_only", Reason: ReasonPlanOnly, EvidenceID: decision.EvidenceID,
		Plan: planned,
	}, "")
}

func (executor *ProductionIntentExecutor) findCandidates(
	ctx context.Context, request policy.ActionRequest,
) (CandidateOutcome, error) {
	var input struct {
		QueryID int64  `json:"query_id"`
		Schema  string `json:"schema"`
	}
	if err := json.Unmarshal(request.Arguments, &input); err != nil {
		return CandidateOutcome{}, fmt.Errorf("decode candidate request: %w", err)
	}
	kind := CandidateOptimizeQuery
	if request.Contract.ActionType == "ensure_fk_indexes" {
		kind = CandidateForeignKeyIndex
	}
	candidates, err := executor.store.FindChangeCandidates(ctx, CandidateQuery{
		Kind: kind, QueryID: input.QueryID, Schema: strings.TrimSpace(input.Schema),
	})
	if err != nil {
		return CandidateOutcome{}, err
	}
	return CandidateOutcome{Verdict: "recommend_only", Candidates: candidates}, nil
}

func (executor *ProductionIntentExecutor) authorizeCandidates(
	ctx context.Context, request policy.ActionRequest,
) (CandidateOutcome, error) {
	outcome, err := executor.findCandidates(ctx, request)
	if err != nil {
		return CandidateOutcome{}, err
	}
	feature := "index"
	if request.Contract.ActionType == "ensure_fk_indexes" {
		feature = "fk_index"
	}
	for index := range outcome.Candidates {
		candidate := &outcome.Candidates[index]
		decision := executor.gate.Authorize(ctx, policy.ActionRequest{
			DatabaseID: request.DatabaseID, Feature: feature, SQL: candidate.SQL,
			TargetObjs: []string{candidate.Object}, Contract: candidateIndexContract(),
		})
		candidate.Decision = decisionResult(decision.Verdict)
		candidate.EvidenceID = decision.EvidenceID
	}
	return outcome, nil
}

func (executor *ProductionIntentExecutor) authorizeMigration(
	ctx context.Context, request policy.ActionRequest,
) (MigrationOutcome, error) {
	input, planned, err := executor.buildMigrationPlan(ctx, request)
	if err != nil {
		return MigrationOutcome{}, err
	}
	actions, blocked := executor.preflightMigration(ctx, request, input, planned)
	if blocked != nil {
		return executor.recordMigrationPlan(ctx, request, input, MigrationOutcome{
			Verdict: "blocked", Reason: string(blocked.Reason), EvidenceID: blocked.EvidenceID,
			Plan: planned, Actions: actions,
		}, string(blocked.Reason))
	}
	if executor.migrationRuntime != nil {
		return executor.runMigration(ctx, request, input, planned)
	}
	evidenceID := ""
	if len(actions) > 0 {
		evidenceID = actions[0].EvidenceID
	}
	return executor.recordMigrationPlan(ctx, request, input, MigrationOutcome{
		Verdict: "recommend_only", Reason: ReasonNoRehearsalRuntime, EvidenceID: evidenceID,
		Plan: planned, Actions: actions,
	}, "")
}

// recordMigrationPlan writes a migration plan that never reaches the
// rehearsal runtime to sage.migration_run. A plan without gate evidence
// gets a fresh evidence id: a shared literal made every such run update
// one row (evidence_id is unique) and hid the runs after the first.
func (executor *ProductionIntentExecutor) recordMigrationPlan(
	ctx context.Context, request policy.ActionRequest, input migrationInput,
	outcome MigrationOutcome, reason string,
) (MigrationOutcome, error) {
	if outcome.EvidenceID == "" {
		outcome.EvidenceID = ledger.NewEvidenceID()
	}
	if err := executor.store.RecordMigration(ctx, MigrationRecord{
		DatabaseID: request.DatabaseID, EvidenceID: outcome.EvidenceID,
		SourceSQL: input.sql, Verdict: outcome.Verdict, Reason: reason,
	}); err != nil {
		return MigrationOutcome{}, err
	}
	return outcome, nil
}

// preflightMigration asks the gate about every expand step before any
// clone rehearsal starts (apply_migration goes through the gate like any
// action). It returns the decisions and the first blocking one: a blocked
// step (a hard stop, a confirmed fact, a change class policy forbids)
// ends the request here.
func (executor *ProductionIntentExecutor) preflightMigration(
	ctx context.Context, request policy.ActionRequest, input migrationInput,
	planned plan.Plan,
) ([]ChangeCandidate, *policy.Decision) {
	actions := make([]ChangeCandidate, 0, len(planned.ExpandSteps))
	var blocked *policy.Decision
	for _, step := range planned.ExpandSteps {
		decision := executor.gate.Authorize(ctx, policy.ActionRequest{
			DatabaseID: request.DatabaseID, Feature: "online_migration", SQL: step.SQL,
			TargetObjs: []string{input.table}, Contract: onlineMigrationContract(),
		})
		actions = append(actions, ChangeCandidate{
			Object: input.table, SQL: step.SQL, Decision: decisionResult(decision.Verdict),
			EvidenceID: decision.EvidenceID,
		})
		if decision.Verdict == policy.VerdictBlocked && blocked == nil {
			blocked = &decision
		}
	}
	return actions, blocked
}

// runMigration hands an admitted migration to the rehearsal runtime,
// which rehearses on a clone and authorizes every step again.
func (executor *ProductionIntentExecutor) runMigration(
	ctx context.Context, request policy.ActionRequest, input migrationInput,
	planned plan.Plan,
) (MigrationOutcome, error) {
	result, err := executor.migrationRuntime.Apply(ctx, migrationruntime.Request{
		DatabaseID: request.DatabaseID,
		SQL:        input.sql, Cycle: input.cycle,
		Table: plan.TableFacts{Schema: input.schema, Name: input.name},
	})
	if err != nil {
		return MigrationOutcome{}, fmt.Errorf("run rehearsed migration: %w", err)
	}
	return MigrationOutcome{
		Verdict: string(result.Verdict), Reason: runtimeReason(result),
		EvidenceID:             result.EvidenceID,
		ContractNotBeforeCycle: result.ContractNotBeforeCycle,
		PendingContractSQL:     result.PendingContractSQL,
		ContractScript:         result.ContractScript, Plan: planned,
	}, nil
}

// runtimeReason is the runtime's reason, or the verdict when it gave
// none, so the outcome always says why (migration-06).
func runtimeReason(result migrationruntime.Result) string {
	if result.Reason != "" {
		return result.Reason
	}
	return string(result.Verdict)
}

type migrationInput struct {
	sql    string
	table  string
	schema string
	name   string
	cycle  int
}

func (executor *ProductionIntentExecutor) buildMigrationPlan(
	ctx context.Context, request policy.ActionRequest,
) (migrationInput, plan.Plan, error) {
	var raw struct {
		SQL   string `json:"sql"`
		DDL   string `json:"ddl"`
		Table string `json:"table"`
		Cycle int    `json:"cycle"`
	}
	if err := json.Unmarshal(request.Arguments, &raw); err != nil {
		return migrationInput{}, plan.Plan{}, fmt.Errorf("decode migration: %w", err)
	}
	if raw.SQL == "" {
		raw.SQL = raw.DDL
	}
	schemaName, tableName, err := qualifiedName(raw.Table)
	if err != nil {
		return migrationInput{}, plan.Plan{}, err
	}
	planned, err := executor.planner.Plan(ctx, plan.Request{
		SQL: raw.SQL, Cycle: raw.Cycle,
		Table: plan.TableFacts{Schema: schemaName, Name: tableName},
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return migrationInput{}, plan.Plan{}, fmt.Errorf("build safe migration plan: %w", err)
	}
	if err != nil {
		// Every other planner error refuses the SQL as written (shape,
		// option, name, or a build without the parse tree): the caller
		// must change the request or plan it by hand (migration-02b).
		return migrationInput{}, plan.Plan{}, fmt.Errorf("%w: build safe migration plan: %w",
			ErrIntentInvalid, err)
	}
	return migrationInput{
		sql: raw.SQL, table: raw.Table, schema: schemaName,
		name: tableName, cycle: raw.Cycle,
	}, planned, nil
}

func qualifiedName(value string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: table must be a schema-qualified name",
			ErrIntentInvalid)
	}
	return parts[0], parts[1], nil
}

// candidateIndexContract types a proposed index build; it is undone by
// dropping the index.
func candidateIndexContract() *policy.ActionContract {
	return &policy.ActionContract{
		ActionType: "create_index", RiskTier: policy.RiskSafe,
		RollbackClass: policy.RollbackReversible,
	}
}

// onlineMigrationContract types one expand step of an online migration,
// which can only be fixed forward.
func onlineMigrationContract() *policy.ActionContract {
	return &policy.ActionContract{
		ActionType: "online_migration", RiskTier: policy.RiskModerate,
		RollbackClass: policy.RollbackForwardFixOnly,
	}
}
