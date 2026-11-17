package mcp

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/verify"
)

type TableContractDeclaration struct {
	DatabaseID *int64
	Schema     string
	Table      string
	AppendOnly bool
	Retention  string
	// RetentionColumn is the owner-declared retention clock; required
	// whenever Retention is set.
	RetentionColumn string
	ExpectedPK      string
	Exemptions      json.RawMessage
	DeclaredBy      string
	EvidenceID      string
}

type ConsumerRegistration struct {
	SlotName         string
	Owner            string
	ConsumerIdentity string
}

type WriteOutcome struct {
	Applied    bool     `json:"applied"`
	EvidenceID string   `json:"evidence_id,omitempty"`
	Object     string   `json:"object"`
	Warnings   []string `json:"warnings,omitempty"`
}

type CandidateKind string

const (
	CandidateOptimizeQuery   CandidateKind = "optimize_query"
	CandidateForeignKeyIndex CandidateKind = "foreign_key_index"
)

type CandidateQuery struct {
	Kind    CandidateKind
	QueryID int64
	Schema  string
}

type ChangeCandidate struct {
	FindingID  int64  `json:"finding_id"`
	Object     string `json:"object"`
	SQL        string `json:"sql"`
	Decision   string `json:"decision,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
}

type CandidateOutcome struct {
	Verdict    string            `json:"verdict"`
	Candidates []ChangeCandidate `json:"candidates"`
}

type MigrationRecord struct {
	DatabaseID *int64
	EvidenceID string
	SourceSQL  string
	Verdict    string
	// Reason is the gate's reason code for a blocked plan (empty otherwise).
	Reason string
}

type MigrationOutcome struct {
	Verdict string `json:"verdict"`
	// Reason says why the verdict is what it is; it is always set
	// (migration-06): the gate's reason, the rehearsal's (clone-cleanup
	// failures included), or contract_pending after an expand.
	Reason                 string `json:"reason"`
	EvidenceID             string `json:"evidence_id,omitempty"`
	ContractNotBeforeCycle int    `json:"contract_not_before_cycle,omitempty"`
	// PendingContractSQL lists the contract steps pg_sage did not run.
	PendingContractSQL []string `json:"pending_contract_sql,omitempty"`
	// ContractScript is a forward-fix script for PendingContractSQL that
	// asserts the expand phase's proof first; pg_sage never runs it.
	ContractScript string            `json:"contract_script,omitempty"`
	Plan           plan.Plan         `json:"plan"`
	Actions        []ChangeCandidate `json:"actions,omitempty"`
	// AffectedQueries is what the clone rehearsal measured of the
	// statements that reference the table, and Ladder its tier-2 record
	// with the clone receipt (lab-04b).
	AffectedQueries []rehearsal.QueryMeasurement `json:"affected_queries,omitempty"`
	Ladder          *verify.LadderRecord         `json:"ladder,omitempty"`
}

// Reasons for apply_migration outcomes that never reach the rehearsal
// runtime.
const (
	// ReasonPlanOnly: the request was planned, not rehearsed or applied.
	ReasonPlanOnly = "plan_only"
	// ReasonNoRehearsalRuntime: no clone provider is configured, so the
	// plan is a recommendation.
	ReasonNoRehearsalRuntime = "no_rehearsal_runtime"
)

type MigrationRuntime interface {
	Apply(context.Context, migrationruntime.Request) (migrationruntime.Result, error)
}

type GuaranteeStatus struct {
	XID    map[string]any `json:"xid"`
	WAL    map[string]any `json:"wal"`
	Schema map[string]any `json:"schema"`
}

type IntentStore interface {
	DeclareTableContract(
		context.Context, TableContractDeclaration,
	) (WriteOutcome, error)
	RegisterConsumer(context.Context, ConsumerRegistration) (WriteOutcome, error)
	FindChangeCandidates(context.Context, CandidateQuery) ([]ChangeCandidate, error)
	RecordMigration(context.Context, MigrationRecord) error
}
