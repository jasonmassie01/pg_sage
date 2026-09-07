package mcp

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
)

type TableContractDeclaration struct {
	DatabaseID *int64
	Schema     string
	Table      string
	AppendOnly bool
	Retention  string
	ExpectedPK string
	Exemptions json.RawMessage
	DeclaredBy string
	EvidenceID string
}

type ConsumerRegistration struct {
	SlotName         string
	Owner            string
	ConsumerIdentity string
}

type WriteOutcome struct {
	Applied    bool   `json:"applied"`
	EvidenceID string `json:"evidence_id,omitempty"`
	Object     string `json:"object"`
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
}

type MigrationOutcome struct {
	Verdict                string            `json:"verdict"`
	EvidenceID             string            `json:"evidence_id,omitempty"`
	ContractNotBeforeCycle int               `json:"contract_not_before_cycle,omitempty"`
	Plan                   plan.Plan         `json:"plan"`
	Actions                []ChangeCandidate `json:"actions,omitempty"`
}

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
