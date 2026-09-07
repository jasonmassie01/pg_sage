package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/value"
)

func (access *PostgresAccess) DeclareTableContract(
	ctx context.Context, declaration TableContractDeclaration,
) (WriteOutcome, error) {
	if access == nil || access.pool == nil {
		return WriteOutcome{}, ErrProductionDependencyUnavailable
	}
	exemptions := declaration.Exemptions
	if len(exemptions) == 0 || string(exemptions) == "null" {
		exemptions = json.RawMessage(`[]`)
	}
	_, err := access.pool.Exec(ctx, `INSERT INTO sage.table_contract
		(database_id, schema_name, table_name, append_only, retention_interval,
		 expected_pk, exemptions, declared_by, evidence_id)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::interval,NULLIF($6,''),$7,$8,$9)
		ON CONFLICT (database_id, schema_name, table_name) DO UPDATE SET
		append_only=EXCLUDED.append_only, retention_interval=EXCLUDED.retention_interval,
		expected_pk=EXCLUDED.expected_pk, exemptions=EXCLUDED.exemptions,
		declared_by=EXCLUDED.declared_by, evidence_id=EXCLUDED.evidence_id,
		updated_at=now()`, declaration.DatabaseID, declaration.Schema,
		declaration.Table, declaration.AppendOnly, declaration.Retention,
		declaration.ExpectedPK, exemptions, declaration.DeclaredBy,
		declaration.EvidenceID)
	if err != nil {
		return WriteOutcome{}, fmt.Errorf("declare table contract: %w", err)
	}
	return WriteOutcome{Applied: true, EvidenceID: declaration.EvidenceID,
		Object: declaration.Schema + "." + declaration.Table}, nil
}

func (access *PostgresAccess) RegisterConsumer(
	ctx context.Context, registration ConsumerRegistration,
) (WriteOutcome, error) {
	if access == nil || access.pool == nil {
		return WriteOutcome{}, ErrProductionDependencyUnavailable
	}
	_, err := access.pool.Exec(ctx, `INSERT INTO sage.slot_consumer_registry
		(slot_name, owner_tag, consumer_identity, registered)
		VALUES ($1,$2,NULLIF($3,''),true)
		ON CONFLICT (slot_name) DO UPDATE SET owner_tag=EXCLUDED.owner_tag,
		consumer_identity=EXCLUDED.consumer_identity, registered=true,
		last_confirmed_at=now(), updated_at=now()`, registration.SlotName,
		registration.Owner, registration.ConsumerIdentity)
	if err != nil {
		return WriteOutcome{}, fmt.Errorf("register slot consumer: %w", err)
	}
	return WriteOutcome{Applied: true, Object: registration.SlotName}, nil
}

func (access *PostgresAccess) FindChangeCandidates(
	ctx context.Context, query CandidateQuery,
) ([]ChangeCandidate, error) {
	if access == nil || access.pool == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	if query.Kind == CandidateOptimizeQuery {
		return access.queryCandidates(ctx, `category='query_optimization'
			AND detail->>'query_id'=$1`, fmt.Sprint(query.QueryID))
	}
	if query.Kind == CandidateForeignKeyIndex {
		return access.queryCandidates(ctx, `category='missing_fk_index'
			AND ($1='' OR object_identifier LIKE $1 || '.%')`, query.Schema)
	}
	return nil, errors.New("unsupported change candidate kind")
}

func (access *PostgresAccess) queryCandidates(
	ctx context.Context, predicate string, argument any,
) ([]ChangeCandidate, error) {
	query := `SELECT id, COALESCE(object_identifier,''), recommended_sql
		FROM sage.findings WHERE status='open' AND recommended_sql IS NOT NULL AND ` +
		predicate + ` ORDER BY last_seen DESC LIMIT 100`
	rows, err := access.pool.Query(ctx, query, argument)
	if err != nil {
		return nil, fmt.Errorf("read MCP change candidates: %w", err)
	}
	defer rows.Close()
	result := make([]ChangeCandidate, 0)
	for rows.Next() {
		var candidate ChangeCandidate
		if err := rows.Scan(&candidate.FindingID, &candidate.Object, &candidate.SQL); err != nil {
			return nil, fmt.Errorf("scan MCP change candidate: %w", err)
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MCP change candidates: %w", err)
	}
	return result, nil
}

func (access *PostgresAccess) RecordMigration(
	ctx context.Context, record MigrationRecord,
) error {
	if access == nil || access.pool == nil {
		return ErrProductionDependencyUnavailable
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(record.SourceSQL)))
	_, err := access.pool.Exec(ctx, `INSERT INTO sage.migration_run
		(database_id, evidence_id, phase, source_sql_hash, verdict)
		VALUES ($1,$2,'plan',$3,$4)
		ON CONFLICT (evidence_id) DO UPDATE SET verdict=EXCLUDED.verdict,
		updated_at=now()`, record.DatabaseID, record.EvidenceID, hash, record.Verdict)
	if err != nil {
		return fmt.Errorf("record MCP migration plan: %w", err)
	}
	return nil
}

func (access *PostgresAccess) GetGuaranteeStatus(
	ctx context.Context,
) (GuaranteeStatus, error) {
	if access == nil || access.pool == nil {
		return GuaranteeStatus{}, ErrProductionDependencyUnavailable
	}
	var oldestAge, consumers, contracts int64
	if err := access.pool.QueryRow(ctx,
		`SELECT COALESCE(max(age(datfrozenxid)),0)::bigint FROM pg_database`,
	).Scan(&oldestAge); err != nil {
		return GuaranteeStatus{}, fmt.Errorf("read XID guarantee: %w", err)
	}
	if err := access.pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.slot_consumer_registry WHERE registered`,
	).Scan(&consumers); err != nil {
		return GuaranteeStatus{}, fmt.Errorf("read WAL guarantee: %w", err)
	}
	if err := access.pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.table_contract`,
	).Scan(&contracts); err != nil {
		return GuaranteeStatus{}, fmt.Errorf("read schema guarantee: %w", err)
	}
	return GuaranteeStatus{
		XID:    map[string]any{"oldest_database_age": oldestAge},
		WAL:    map[string]any{"registered_consumers": consumers},
		Schema: map[string]any{"declared_contracts": contracts},
	}, nil
}

type PostgresAccess struct {
	pool     *pgxpool.Pool
	policies *policy.Store
	value    *value.Service
}

func NewPostgresAccess(pool *pgxpool.Pool) *PostgresAccess {
	return &PostgresAccess{
		pool:     pool,
		policies: policy.NewStore(pool),
		value:    value.NewService(value.NewPostgresRepository(pool)),
	}
}

func (access *PostgresAccess) GetPolicy(
	ctx context.Context, request PolicyRequest,
) (PolicyResult, error) {
	if access == nil || access.pool == nil {
		return PolicyResult{}, ErrProductionDependencyUnavailable
	}
	current, err := access.policies.Current(
		ctx, policy.Scope{DatabaseID: request.DatabaseID},
	)
	if err != nil {
		return PolicyResult{}, fmt.Errorf("read MCP policy: %w", err)
	}
	return PolicyResult{
		DatabaseID: request.DatabaseID,
		Version:    current.Version,
		Profile:    current.Profile,
	}, nil
}

func (access *PostgresAccess) ProposePolicyChangeDryRun(
	ctx context.Context, request PolicyProposalRequest,
) (PolicyProposalResult, error) {
	if access == nil || access.pool == nil {
		return PolicyProposalResult{}, ErrProductionDependencyUnavailable
	}
	scope := policy.Scope{DatabaseID: request.DatabaseID}
	current, err := access.policies.Current(ctx, scope)
	if err != nil {
		return PolicyProposalResult{}, fmt.Errorf("read policy for proposal: %w", err)
	}
	document, err := mergePolicyDelta(current.Document, request.Delta)
	if err != nil {
		return PolicyProposalResult{}, err
	}
	proposal, err := access.policies.Propose(ctx, policy.ProposalRequest{
		Scope: scope, ExpectedVersion: current.Version,
		Profile: current.Profile, Document: document,
		Actor: "mcp-agent-proposal", Preview: policy.ImpactPreview{},
	})
	if err != nil {
		return PolicyProposalResult{}, fmt.Errorf("persist dry-run policy proposal: %w", err)
	}
	return PolicyProposalResult{
		ProposalID: proposal.ID,
		DryRunImpact: DryRunImpact{
			NewlyAllowed: outcomeIDs(proposal.Preview.ChangedPendingOutcomes, "allow"),
			NewlyBlocked: outcomeIDs(proposal.Preview.ChangedPendingOutcomes, "block"),
		},
	}, nil
}

func (access *PostgresAccess) GetLedger(
	ctx context.Context, request LedgerRequest,
) (LedgerResult, error) {
	if access == nil || access.pool == nil {
		return LedgerResult{}, ErrProductionDependencyUnavailable
	}
	filter, err := parseLedgerFilter(request.Filter)
	if err != nil {
		return LedgerResult{}, err
	}
	rows, err := access.pool.Query(ctx, `SELECT evidence_id, verdict, feature
		FROM sage.decision
		WHERE ($1::int IS NULL OR database_id=$1)
		AND ($2='' OR verdict=$2) AND ($3='' OR feature=$3)
		ORDER BY created_at DESC LIMIT $4`,
		filter.DatabaseID, filter.Decision, filter.Feature, filter.Limit)
	if err != nil {
		return LedgerResult{}, fmt.Errorf("read MCP ledger: %w", err)
	}
	defer rows.Close()
	entries := make([]LedgerEntry, 0)
	for rows.Next() {
		var entry LedgerEntry
		if err := rows.Scan(&entry.EvidenceID, &entry.Decision, &entry.Feature); err != nil {
			return LedgerResult{}, fmt.Errorf("scan MCP ledger: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return LedgerResult{}, fmt.Errorf("iterate MCP ledger: %w", err)
	}
	return LedgerResult{Entries: entries}, nil
}

func (access *PostgresAccess) GetValue(ctx context.Context) (map[string]any, error) {
	if access == nil || access.pool == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	report, err := access.value.Get(ctx, value.Filter{})
	if err != nil {
		return nil, fmt.Errorf("read MCP value: %w", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("encode MCP value: %w", err)
	}
	result := make(map[string]any)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode MCP value: %w", err)
	}
	return result, nil
}

type ledgerFilter struct {
	DatabaseID *int64 `json:"database_id"`
	Decision   string `json:"decision"`
	Feature    string `json:"feature"`
	Limit      int    `json:"limit"`
}

func parseLedgerFilter(raw json.RawMessage) (ledgerFilter, error) {
	result := ledgerFilter{Limit: 100}
	if len(raw) == 0 || string(raw) == "null" {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ledgerFilter{}, fmt.Errorf("decode MCP ledger filter: %w", err)
	}
	if result.Limit <= 0 || result.Limit > 1000 {
		return ledgerFilter{}, errors.New("MCP ledger limit must be within 1-1000")
	}
	return result, nil
}

func mergePolicyDelta(
	current json.RawMessage, delta json.RawMessage,
) (json.RawMessage, error) {
	var base map[string]any
	var patch map[string]any
	if json.Unmarshal(current, &base) != nil || base == nil {
		return nil, policy.ErrInvalidDocument
	}
	if json.Unmarshal(delta, &patch) != nil || patch == nil {
		return nil, policy.ErrInvalidDocument
	}
	mergeObjects(base, patch)
	result, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("encode merged policy: %w", err)
	}
	if _, err := policy.ParseDocument(result); err != nil {
		return nil, err
	}
	return result, nil
}

func mergeObjects(target map[string]any, patch map[string]any) {
	for key, value := range patch {
		patchObject, patchOK := value.(map[string]any)
		targetObject, targetOK := target[key].(map[string]any)
		if patchOK && targetOK {
			mergeObjects(targetObject, patchObject)
			continue
		}
		target[key] = value
	}
}

func outcomeIDs(changes []policy.PendingOutcomeChange, direction string) []string {
	result := make([]string, 0)
	for _, change := range changes {
		matches := direction == "allow" && strings.EqualFold(change.After, "execute")
		matches = matches || direction == "block" &&
			!strings.EqualFold(change.After, "execute")
		if matches {
			result = append(result, fmt.Sprintf("action-%d", change.ActionID))
		}
	}
	return result
}
