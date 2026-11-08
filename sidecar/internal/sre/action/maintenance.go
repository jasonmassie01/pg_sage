package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

const MaintenanceIdentityPrefix = "sre_maintenance:"

// StatisticsRequest is an explicit operator hypothesis, never a model-selected
// action. stat_tables evidence establishes the table's observed scope; it does
// not prove column correlation or a performance benefit.
type StatisticsRequest struct {
	Schema         string   `json:"schema"`
	Table          string   `json:"table"`
	Columns        []string `json:"columns"`
	Kinds          []string `json:"kinds"`
	EvidenceID     sre.UUID `json:"evidence_id"`
	EvidenceSHA256 string   `json:"evidence_sha256"`
	Rationale      string   `json:"rationale"`
}
type MaintenanceProposal struct {
	ID              sre.UUID                      `json:"id"`
	InvestigationID sre.UUID                      `json:"investigation_id"`
	Class           string                        `json:"class"`
	State           string                        `json:"state"`
	Request         StatisticsRequest             `json:"request"`
	Target          executor.StatisticsTarget     `json:"target"`
	TargetSHA256    string                        `json:"target_sha256"`
	SQL             string                        `json:"sql"`
	RollbackSQL     string                        `json:"rollback_sql"`
	Contract        executor.RepairContract       `json:"contract"`
	Policy          executor.ActionPolicyDecision `json:"policy"`
	QueueID         int                           `json:"queue_id"`
	FindingID       int                           `json:"finding_id"`
	ActionLogID     int64                         `json:"action_log_id"`
	StatisticsOID   uint32                        `json:"statistics_oid"`
	RequestedBy     string                        `json:"requested_by"`
	ApprovedBy      int                           `json:"approved_by"`
	CreatedAt       time.Time                     `json:"created_at"`
	ExpiresAt       time.Time                     `json:"expires_at"`
	Verification    string                        `json:"verification"`
	Detail          string                        `json:"detail,omitempty"`
}
type MaintenanceDeps struct {
	Service        *sre.Service
	Pool           *pgxpool.Pool
	Executor       *executor.Executor
	DatabaseID     *int
	ApprovalTTL    time.Duration
	MaxEvidenceAge time.Duration
}
type MaintenanceService struct {
	MaintenanceDeps
	st *sre.PostgresStore
}

func NewMaintenanceService(d MaintenanceDeps) (*MaintenanceService, error) {
	if d.Service == nil || d.Service.Coordinator() == nil || d.Service.Store() == nil || d.Pool == nil || d.Executor == nil {
		return nil, fmt.Errorf("%w: maintenance needs investigations, target pool and executor", sre.ErrInvalidRequest)
	}
	// Queue, action and audit retention must see the same maintenance ledger.
	// Cross-database lifetime holds are not implemented; refuse that topology
	// before a proposal can be created rather than leave dangling rollback IDs.
	if probes.PoolBinding(d.Service.Store().Pool()) != probes.PoolBinding(d.Pool) {
		return nil, fmt.Errorf("%w: maintenance requires colocated coordination and target databases", sre.ErrInvalidRequest)
	}
	if !d.Executor.StatisticsPoolMatches(d.Pool) {
		return nil, fmt.Errorf("%w: maintenance and executor target pools differ", sre.ErrInvalidRequest)
	}
	if d.ApprovalTTL == 0 {
		d.ApprovalTTL = 15 * time.Minute
	}
	if d.MaxEvidenceAge == 0 {
		d.MaxEvidenceAge = 15 * time.Minute
	}
	if d.ApprovalTTL <= 0 || d.ApprovalTTL > 15*time.Minute || d.MaxEvidenceAge <= 0 || d.MaxEvidenceAge > 15*time.Minute {
		return nil, fmt.Errorf("%w: maintenance TTL and evidence age must be within 15 minutes", sre.ErrInvalidRequest)
	}
	return &MaintenanceService{MaintenanceDeps: d, st: d.Service.Store()}, nil
}
func (m *MaintenanceService) allowed() error {
	if !m.Executor.StatisticsPoolMatches(m.Pool) {
		return fmt.Errorf("%w: maintenance executor target pool changed", sre.ErrInvalidRequest)
	}
	if !m.Service.Coordinator().Durability().HandoffAllowed() {
		return ErrHandoffBlocked
	}
	return nil
}
func (m *MaintenanceService) scope(ctx context.Context) (sre.Scope, error) {
	return m.Service.Scope(ctx)
}
func (m *MaintenanceService) Get(ctx context.Context, id sre.UUID) (MaintenanceProposal, error) {
	scope, err := m.scope(ctx)
	if err != nil {
		return MaintenanceProposal{}, err
	}
	var raw []byte
	err = m.st.Pool().QueryRow(ctx, `SELECT proposal FROM sage.sre_maintenance_proposals WHERE deployment_id=$1 AND database_id=$2 AND id=$3`, string(scope.DeploymentID), string(scope.DatabaseID), string(id)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return MaintenanceProposal{}, ErrProposalNotFound
	}
	if err != nil {
		return MaintenanceProposal{}, err
	}
	var p MaintenanceProposal
	err = json.Unmarshal(raw, &p)
	return p, err
}
func (m *MaintenanceService) ForInvestigation(ctx context.Context, id sre.UUID) ([]MaintenanceProposal, error) {
	scope, err := m.scope(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := m.st.Pool().Query(ctx, `SELECT proposal FROM sage.sre_maintenance_proposals WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3`, string(scope.DeploymentID), string(scope.DatabaseID), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MaintenanceProposal{}
	for rows.Next() {
		var raw []byte
		var p MaintenanceProposal
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (m *MaintenanceService) evidence(ctx context.Context, invID sre.UUID, req StatisticsRequest, target *executor.StatisticsTarget) error {
	scope, err := m.scope(ctx)
	if err != nil {
		return err
	}
	inv, err := m.st.Get(ctx, scope, invID)
	if err != nil {
		return err
	}
	if inv.State != sre.StateConcluded || !inv.EvidencePurgedAt.IsZero() {
		return fmt.Errorf("%w: maintenance needs a concluded investigation with retained evidence", ErrProposalState)
	}
	if err = m.st.VerifyEvents(ctx, scope, invID); err != nil {
		return err
	}
	pinned, err := m.st.InvestigationReadPolicy(ctx, scope, invID)
	if err != nil {
		return err
	}
	current := m.Service.Coordinator().ReadPolicy()
	if pinned.Hash() != current.Hash() {
		return fmt.Errorf("%w: trusted read policy changed", sre.ErrInvalidRequest)
	}
	if pinned != nil && (pinned.Database != m.Pool.Config().ConnConfig.Database || pinned.Binding != probes.PoolBinding(m.Pool) || !pinned.Allows(probes.StatTables) || pinned.QueryID != 0) {
		return fmt.Errorf("%w: maintenance pool is outside investigation read policy", sre.ErrInvalidRequest)
	}

	e, err := m.st.EvidenceByID(ctx, scope, invID, req.EvidenceID)
	if err != nil {
		return err
	}
	age := time.Since(e.ObservedAt)
	if !e.VerifyHash() || hex.EncodeToString(e.SHA256) != req.EvidenceSHA256 || e.ProbeID != string(probes.StatTables) || e.CapabilityState != "available" || age < -time.Second || age > m.MaxEvidenceAge {
		return fmt.Errorf("%w: requires fresh hash-verified stat_tables evidence", sre.ErrInvalidRequest)
	}
	var res probes.Result
	if err = json.Unmarshal(e.Payload, &res); err != nil {
		return err
	}
	if res.ProbeID != probes.StatTables || res.Status != probes.StatusOK {
		return sre.ErrInvalidRequest
	}
	if target == nil {
		return nil
	}
	for _, row := range res.Rows {
		databaseOID, databaseOK := row["database_oid"].(float64)
		tableOID, tableOK := row["table_oid"].(float64)
		if row["relation"] == target.Relation && databaseOK && tableOK && databaseOID == float64(target.DatabaseOID) && tableOID == float64(target.TableOID) {
			return nil
		}
	}
	return fmt.Errorf("%w: evidence does not contain the requested physical database and table identity", sre.ErrInvalidRequest)
}
func (m *MaintenanceService) ProposeStatistics(ctx context.Context, invID sre.UUID, req StatisticsRequest, actor string) (MaintenanceProposal, error) {
	if err := m.allowed(); err != nil {
		return MaintenanceProposal{}, err
	}
	if actor == "" || len(actor) > 128 || len(strings.TrimSpace(req.Rationale)) < 10 || len(req.Rationale) > 2048 {
		return MaintenanceProposal{}, sre.ErrInvalidRequest
	}
	id := sre.NewUUID()
	name := "sage_stx_" + strings.ReplaceAll(string(id), "-", "")
	cols := make([]string, len(req.Columns))
	for i, c := range req.Columns {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	if len(req.Kinds) == 0 {
		req.Kinds = []string{"dependencies", "ndistinct"}
	}
	sql := "CREATE STATISTICS " + pgx.Identifier{req.Schema, name}.Sanitize() + " (" + strings.Join(req.Kinds, ", ") + ") ON " + strings.Join(cols, ", ") + " FROM " + pgx.Identifier{req.Schema, req.Table}.Sanitize()
	c, err := extstats.ParseCreate(sql)
	if err != nil {
		return MaintenanceProposal{}, fmt.Errorf("%w: %v", sre.ErrInvalidRequest, err)
	}
	if err = m.evidence(ctx, invID, req, nil); err != nil {
		return MaintenanceProposal{}, err
	}
	target, err := executor.CaptureStatisticsTarget(ctx, m.Pool, req.Schema, req.Table, req.Columns)
	if err != nil {
		if errors.Is(err, executor.ErrStatisticsTargetInvalid) {
			return MaintenanceProposal{}, fmt.Errorf("%w: requested table or columns are unavailable", sre.ErrInvalidRequest)
		}
		return MaintenanceProposal{}, fmt.Errorf("%w: target catalog read unavailable", sre.ErrMetadataUnavailable)
	}
	if err = m.evidence(ctx, invID, req, &target); err != nil {
		return MaintenanceProposal{}, err
	}
	p := MaintenanceProposal{ID: id, InvestigationID: invID, Class: "create_statistics", State: "proposed", Request: req, Target: target, SQL: sql, RollbackSQL: c.Rollback(), RequestedBy: actor, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(m.ApprovalTTL), Verification: "not_started"}
	scope, err := m.scope(ctx)
	if err != nil {
		return p, err
	}
	ev, err := m.st.EvidenceByID(ctx, scope, invID, req.EvidenceID)
	if err != nil {
		return p, err
	}
	if until := ev.ObservedAt.Add(m.MaxEvidenceAge); until.Before(p.ExpiresAt) {
		p.ExpiresAt = until
	}
	raw, _ := json.Marshal(target)
	digest := sha256.Sum256(raw)
	p.TargetSHA256 = hex.EncodeToString(digest[:])
	p.Contract = statisticsRepairContract(p)
	p.Policy = m.Executor.PreviewStatistics(ctx, p.SQL)
	err = m.st.WithInvestigationLock(ctx, scope, invID, actor, func(tx pgx.Tx, emit sre.Emit) error {
		var old []byte
		err := tx.QueryRow(ctx, `SELECT proposal FROM sage.sre_maintenance_proposals WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3`, string(scope.DeploymentID), string(scope.DatabaseID), string(invID)).Scan(&old)
		if err == nil {
			var prior MaintenanceProposal
			if err = json.Unmarshal(old, &prior); err != nil {
				return err
			}
			a, _ := json.Marshal(prior.Request)
			b, _ := json.Marshal(req)
			if string(a) != string(b) {
				return fmt.Errorf("%w: this investigation already has a different maintenance request", ErrProposalState)
			}
			p = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO sage.sre_maintenance_proposals(id,deployment_id,database_id,investigation_id,state,proposal) VALUES($1,$2,$3,$4,$5,$6)`, string(p.ID), string(scope.DeploymentID), string(scope.DatabaseID), string(invID), p.State, raw)
		if err != nil {
			return err
		}
		return emit("action_proposed", map[string]any{"proposal_id": p.ID, "class": p.Class, "operator_requested": true, "evidence_id": req.EvidenceID, "target_sha256": p.TargetSHA256, "contract": p.Contract})
	})
	return p, err
}
func statisticsRepairContract(p MaintenanceProposal) executor.RepairContract {
	base, _ := executor.ContractForActionType("create_statistics")
	base.RequiredPermissions = []string{"CREATE privilege on the statistics schema", "ownership of the target table"}
	base.Prechecks = []string{"fresh retained same-table counters and explicit operator correlation hypothesis", "bound table and column identities unchanged; target object absent", "human approval and live policy allow this change"}
	base.SuccessCriteria = []string{"newly created statistics object exists on the bound table and is built; structural result only"}
	base.PostChecks = []string{"capture created statistics OID and verify built statistics in the mutation transaction", "retain workload benefit and incident recovery as unproven"}

	return executor.RepairContract{ActionContract: base, Version: "sre-statistics-v1", Preconditions: []string{"human approval; fresh retained same-table stat_tables evidence", "table OID, owner and column identities unchanged under table lock", "live policy, trust and emergency-stop checks"}, Scope: "one table observed in the cited evidence; columns are an explicit operator hypothesis", LockImpact: "SHARE UPDATE EXCLUSIVE; reads and writes continue; bounded lock wait", Timeouts: []string{"executor statement timeout and standing-policy lock timeout"}, Reversibility: executor.ReversibilityReversible, RollbackTrigger: "explicit human request; no automatic maintenance rollback", Inverse: p.RollbackSQL, PostConditions: []string{"created statistics object exists on bound table and is built; structural verification only"}, BlastRadius: executor.BlastRadiusBudget{MaxBackends: 1, MaxDatabases: 1, MaxActionsPerInvestigation: 1}, NeverDo: []string{"customer data writes", "CASCADE", "replace an existing statistics object", "infer correlation or performance improvement from table counters"}, ResidualRisk: "ANALYZE refreshes planner statistics. Rollback removes only the newly created object and cannot restore prior ANALYZE statistics. Structural verification does not establish workload improvement or incident recovery."}
}
