package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
		return WriteOutcome{}, fmt.Errorf("decode table contract: %w", err)
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
		return WriteOutcome{}, fmt.Errorf("decode consumer registration: %w", err)
	}
	if strings.TrimSpace(input.SlotName) == "" || strings.TrimSpace(input.Owner) == "" {
		return WriteOutcome{}, errors.New("slot_name and owner are required")
	}
	return executor.store.RegisterConsumer(ctx, ConsumerRegistration{
		SlotName: strings.TrimSpace(input.SlotName), Owner: strings.TrimSpace(input.Owner),
		ConsumerIdentity: strings.TrimSpace(input.Identity),
	})
}

func (executor *ProductionIntentExecutor) planMigration(
	ctx context.Context, request policy.ActionRequest, decision policy.Decision,
) (MigrationOutcome, error) {
	var input struct {
		SQL   string `json:"sql"`
		DDL   string `json:"ddl"`
		Table string `json:"table"`
		Cycle int    `json:"cycle"`
	}
	if err := json.Unmarshal(request.Arguments, &input); err != nil {
		return MigrationOutcome{}, fmt.Errorf("decode migration: %w", err)
	}
	if input.SQL == "" {
		input.SQL = input.DDL
	}
	schemaName, tableName, err := qualifiedName(input.Table)
	if err != nil {
		return MigrationOutcome{}, err
	}
	planned, err := executor.planner.Plan(ctx, plan.Request{
		SQL: input.SQL, Cycle: input.Cycle,
		Table: plan.TableFacts{Schema: schemaName, Name: tableName},
	})
	if err != nil {
		return MigrationOutcome{}, fmt.Errorf("build safe migration plan: %w", err)
	}
	const verdict = "recommend_only"
	if err := executor.store.RecordMigration(ctx, MigrationRecord{
		DatabaseID: request.DatabaseID, EvidenceID: decision.EvidenceID,
		SourceSQL: input.SQL, Verdict: verdict,
	}); err != nil {
		return MigrationOutcome{}, err
	}
	return MigrationOutcome{Verdict: verdict, Plan: planned}, nil
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
		if err := executor.store.RecordMigration(ctx, MigrationRecord{
			DatabaseID: request.DatabaseID, EvidenceID: blocked.EvidenceID,
			SourceSQL: input.sql, Verdict: "blocked",
		}); err != nil {
			return MigrationOutcome{}, err
		}
		return MigrationOutcome{Verdict: "blocked", EvidenceID: blocked.EvidenceID,
			Plan: planned, Actions: actions}, nil
	}
	if executor.migrationRuntime != nil {
		return executor.runMigration(ctx, request, input, planned)
	}
	evidenceID := "migration-recommendation"
	if len(actions) > 0 && actions[0].EvidenceID != "" {
		evidenceID = actions[0].EvidenceID
	}
	if err := executor.store.RecordMigration(ctx, MigrationRecord{
		DatabaseID: request.DatabaseID, EvidenceID: evidenceID,
		SourceSQL: input.sql, Verdict: "recommend_only",
	}); err != nil {
		return MigrationOutcome{}, err
	}
	return MigrationOutcome{
		Verdict: "recommend_only", Plan: planned, Actions: actions,
	}, nil
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
		Verdict: string(result.Verdict), EvidenceID: result.EvidenceID,
		ContractNotBeforeCycle: result.ContractNotBeforeCycle, Plan: planned,
	}, nil
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
	if err != nil {
		return migrationInput{}, plan.Plan{}, fmt.Errorf("build safe migration plan: %w", err)
	}
	return migrationInput{
		sql: raw.SQL, table: raw.Table, schema: schemaName,
		name: tableName, cycle: raw.Cycle,
	}, planned, nil
}

func qualifiedName(value string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("table must be a schema-qualified name")
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
