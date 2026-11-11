package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Statistics maintenance tools (playbooks-06) resolve the database's
// maintenance service through the fleet. The service resolves the cited
// evidence; approval happens in the existing flow.

var _ mcp.SREMaintenanceBackend = (*fleetMCPAccess)(nil)

// refreshedEvidence is what sre_refresh_table_evidence returns.
type refreshedEvidence struct {
	Database        string    `json:"database"`
	InvestigationID sre.UUID  `json:"investigation_id"`
	EvidenceID      sre.UUID  `json:"evidence_id"`
	Step            string    `json:"step"`
	ObservedAt      time.Time `json:"observed_at"`
	FreshUntil      time.Time `json:"fresh_until"`
	SHA256          string    `json:"sha256"`
}

// RefreshTableEvidence re-reads stat_tables for a table the investigation
// observed.
func (access *fleetMCPAccess) RefreshTableEvidence(ctx context.Context,
	r mcp.SREMaintenanceRequest) (any, error) {
	m, err := access.maintenanceService(r.Database)
	if err != nil {
		return nil, err
	}
	ev, err := m.RefreshTable(ctx, sre.UUID(r.InvestigationID), r.Schema, r.Table,
		mcp.ActorFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return refreshedEvidence{Database: m.Service.Name(),
		InvestigationID: sre.UUID(r.InvestigationID), EvidenceID: ev.ID, Step: ev.StepKey,
		ObservedAt: ev.ObservedAt, FreshUntil: ev.ObservedAt.Add(m.MaxEvidenceAge),
		SHA256: hex.EncodeToString(ev.SHA256)}, nil
}

// ProposeStatistics proposes against the newest matching evidence.
func (access *fleetMCPAccess) ProposeStatistics(ctx context.Context,
	r mcp.SREMaintenanceRequest) (any, error) {
	m, err := access.maintenanceService(r.Database)
	if err != nil {
		return nil, err
	}
	return m.ProposeStatisticsForTable(ctx, sre.UUID(r.InvestigationID),
		sreaction.TableStatistics{Schema: r.Schema, Table: r.Table, Columns: r.Columns,
			Kinds: r.Kinds, Rationale: r.Rationale}, mcp.ActorFromContext(ctx))
}

// RequestStatistics queues a statistics proposal's single approval item.
func (access *fleetMCPAccess) RequestStatistics(ctx context.Context,
	r mcp.SREMaintenanceRequest) (any, error) {
	m, err := access.maintenanceService(r.Database)
	if err != nil {
		return nil, err
	}
	id, err := sre.ParseUUID(r.ProposalID)
	if err != nil {
		return nil, err
	}
	return m.RequestExecution(ctx, id, mcp.ActorFromContext(ctx))
}

// RollbackStatistics drops the object a statistics proposal created; an
// admin person only.
func (access *fleetMCPAccess) RollbackStatistics(ctx context.Context,
	r mcp.SREMaintenanceRequest) (any, error) {
	approver, err := mcp.RollbackApprover(ctx)
	if err != nil {
		return nil, err
	}
	m, err := access.maintenanceService(r.Database)
	if err != nil {
		return nil, err
	}
	id, err := sre.ParseUUID(r.ProposalID)
	if err != nil {
		return nil, err
	}
	return m.Rollback(ctx, id, r.Reason, approver)
}

// maintenanceService is the named database's statistics maintenance
// service; it exists only where stat_tables is allowed and the
// coordination and target databases are colocated.
func (access *fleetMCPAccess) maintenanceService(name string) (*sreaction.MaintenanceService,
	error) {
	a, err := access.actionService(name)
	if err != nil {
		return nil, err
	}
	m := a.MaintenanceService()
	if m == nil {
		return nil, fmt.Errorf("%w: statistics maintenance unavailable for this database",
			sre.ErrMetadataUnavailable)
	}
	return m, nil
}
