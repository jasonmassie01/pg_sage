package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/store"
)

func (m *MaintenanceService) mutate(ctx context.Context, id sre.UUID, actor string, fn func(*MaintenanceProposal, sre.Emit) error) (MaintenanceProposal, error) {
	p, err := m.Get(ctx, id)
	if err != nil {
		return p, err
	}
	scope, err := m.scope(ctx)
	if err != nil {
		return p, err
	}
	err = m.st.WithInvestigationLock(ctx, scope, p.InvestigationID, actor, func(tx pgx.Tx, emit sre.Emit) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT proposal FROM sage.sre_maintenance_proposals WHERE deployment_id=$1 AND database_id=$2 AND id=$3 FOR UPDATE`, string(scope.DeploymentID), string(scope.DatabaseID), string(id)).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if err := fn(&p, emit); err != nil {
			return err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE sage.sre_maintenance_proposals SET state=$4,proposal=$5,updated_at=clock_timestamp() WHERE deployment_id=$1 AND database_id=$2 AND id=$3`, string(scope.DeploymentID), string(scope.DatabaseID), string(id), p.State, raw)
		return err
	})
	return p, err
}

// RequestExecution adds a pending human-approval item. No SQL executes here.
func (m *MaintenanceService) RequestExecution(ctx context.Context, id sre.UUID, actor string) (MaintenanceProposal, error) {
	if err := m.allowed(); err != nil {
		return MaintenanceProposal{}, err
	}
	if actor == "" || len(actor) > 128 {
		return MaintenanceProposal{}, sre.ErrInvalidRequest
	}
	return m.mutate(ctx, id, actor, func(p *MaintenanceProposal, emit sre.Emit) error {
		if p.State == "requested" {
			return nil
		}
		if p.State != "proposed" || !time.Now().Before(p.ExpiresAt) {
			return ErrProposalState
		}
		if err := m.evidence(ctx, p.InvestigationID, p.Request, &p.Target); err != nil {
			return err
		}
		p.Policy = m.Executor.PreviewStatistics(ctx, p.SQL)
		if p.Policy.Decision != executor.PolicyDecisionExecute {
			return fmt.Errorf("%w: %s", ErrPolicyBlocked, p.Policy.Detail)
		}
		if err := m.enqueue(ctx, p); err != nil {
			return err
		}
		p.State = "requested"
		return emit("action_requested", map[string]any{"proposal_id": p.ID, "class": p.Class, "queue_id": p.QueueID, "finding_id": p.FindingID, "requested_by": actor})
	})
}
func (m *MaintenanceService) enqueue(ctx context.Context, p *MaintenanceProposal) error {
	tx, err := m.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	key := MaintenanceIdentityPrefix + string(p.ID)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT id,finding_id FROM sage.action_queue WHERE identity_key=$1`, key).Scan(&p.QueueID, &p.FindingID)
	if err == nil {
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO sage.findings(category,severity,object_type,object_identifier,title,detail,recommendation,recommended_sql,rollback_sql)
 VALUES('sre_maintenance','warning','table',$1,'Operator-requested extended statistics',$2::jsonb,$3,$4,$5) RETURNING id`, key, raw, p.Contract.ResidualRisk, p.SQL, p.RollbackSQL).Scan(&p.FindingID)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO sage.action_queue(database_id,finding_id,proposed_sql,rollback_sql,action_risk,action_type,identity_key,policy_decision,guardrails,expires_at)
 VALUES($1,$2,$3,$4,'moderate','create_statistics',$5,'queue_approval','["human_approval","table_oid_recheck","column_identity_recheck","structural_verification_only","analyze_not_restorable"]'::jsonb,$6) RETURNING id`, m.DatabaseID, p.FindingID, p.SQL, p.RollbackSQL, key, p.ExpiresAt).Scan(&p.QueueID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Owns reserves every maintenance identity so a missing ledger cannot fall
// through to the generic manual path.
func (m *MaintenanceService) Owns(a store.QueuedAction) bool {
	return strings.HasPrefix(a.IdentityKey, MaintenanceIdentityPrefix)
}
func (m *MaintenanceService) RunApproved(ctx context.Context, a store.QueuedAction, approvedBy int) (executor.ApprovedRun, error) {
	if err := m.allowed(); err != nil {
		return executor.ApprovedRun{}, err
	}
	id, err := sre.ParseUUID(strings.TrimPrefix(a.IdentityKey, MaintenanceIdentityPrefix))
	if err != nil || !m.Owns(a) {
		return executor.ApprovedRun{}, ErrProposalNotFound
	}
	p, err := m.mutate(ctx, id, userActor(approvedBy), func(p *MaintenanceProposal, emit sre.Emit) error {
		if p.State != "requested" || approvedBy <= 0 || a.ID != p.QueueID || a.FindingID != p.FindingID || a.ProposedSQL != p.SQL || a.RollbackSQL != p.RollbackSQL || a.ActionType != "create_statistics" {
			return ErrProposalState
		}
		var ok bool
		err := m.Pool.QueryRow(ctx, `SELECT status='approved' AND decided_by=$2 AND expires_at>clock_timestamp() AND finding_id=$3 AND proposed_sql=$4 AND rollback_sql=$5 AND identity_key=$6 AND action_type='create_statistics' AND database_id IS NOT DISTINCT FROM $7 FROM sage.action_queue WHERE id=$1`, p.QueueID, approvedBy, p.FindingID, p.SQL, p.RollbackSQL, MaintenanceIdentityPrefix+string(p.ID), m.DatabaseID).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok || !time.Now().Before(p.ExpiresAt) {
			return ErrProposalState
		}
		if err = m.evidence(ctx, p.InvestigationID, p.Request, &p.Target); err != nil {
			return err
		}
		p.State = "executing"
		p.ApprovedBy = approvedBy
		return emit("action_decided", map[string]any{"proposal_id": p.ID, "class": p.Class, "decision": "approved", "queue_id": p.QueueID, "approved_by": approvedBy})
	})
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	actionID, objectOID, runErr := m.Executor.ExecuteSREStatistics(ctx, p.FindingID, p.SQL, approvedBy, p.Target)
	state, verification, detail := "executed", "verified", "created object is built; workload benefit and incident recovery are unproven"
	if runErr != nil {
		state, verification, detail = "failed", "failed", runErr.Error()
		if actionID > 0 && objectOID > 0 {
			state, verification = "uncertain", "inconclusive"
		}
	}
	_, saveErr := m.mutate(context.WithoutCancel(ctx), id, userActor(approvedBy), func(q *MaintenanceProposal, emit sre.Emit) error {
		if q.State != "executing" {
			return ErrProposalState
		}
		q.State = state
		q.ActionLogID = actionID
		q.StatisticsOID = objectOID
		q.Verification = verification
		q.Detail = detail
		typ := "action_executed"
		if runErr != nil {
			typ = "action_failed"
		}
		return emit(typ, map[string]any{"proposal_id": q.ID, "class": q.Class, "action_log_id": actionID, "statistics_oid": objectOID, "structural_verification": verification, "workload_benefit": "unproven", "incident_recovery": "unproven", "detail": detail})
	})
	if saveErr != nil {
		return executor.ApprovedRun{ActionLogID: actionID, VerificationStatus: "inconclusive"}, saveErr
	}
	_, queueErr := m.Pool.Exec(context.WithoutCancel(ctx), `UPDATE sage.action_queue SET verification_status=$2,action_log_id=NULLIF($3,0) WHERE id=$1`, p.QueueID, verification, actionID)
	if runErr != nil {
		return executor.ApprovedRun{ActionLogID: actionID, VerificationStatus: verification}, runErr
	}
	if queueErr != nil {
		return executor.ApprovedRun{ActionLogID: actionID}, queueErr
	}
	return executor.ApprovedRun{ActionLogID: actionID, VerificationStatus: "verified"}, nil
}

// Rollback is an explicit operator action and uses the existing rollback policy.
func (m *MaintenanceService) Rollback(ctx context.Context, id sre.UUID, reason string, approvedBy int) (MaintenanceProposal, error) {
	if err := m.allowed(); err != nil {
		return MaintenanceProposal{}, err
	}
	if approvedBy <= 0 || strings.TrimSpace(reason) == "" || len(reason) > 1024 {
		return MaintenanceProposal{}, sre.ErrInvalidRequest
	}
	p, err := m.mutate(ctx, id, userActor(approvedBy), func(p *MaintenanceProposal, emit sre.Emit) error {
		if (p.State != "executed" && p.State != "uncertain" && p.State != "rollback_failed") || p.ActionLogID <= 0 || p.StatisticsOID == 0 {
			return ErrProposalState
		}
		p.State = "rolling_back"
		return emit("action_decided", map[string]any{"proposal_id": p.ID, "class": "revert_created_statistics", "decision": "rollback_requested", "requested_by": approvedBy, "reason": reason})
	})
	if err != nil {
		return p, err
	}
	runErr := m.Executor.RollbackSREStatistics(ctx, p.ActionLogID, p.SQL, p.Target, p.StatisticsOID, reason, approvedBy)
	out, saveErr := m.mutate(context.WithoutCancel(ctx), id, userActor(approvedBy), func(p *MaintenanceProposal, emit sre.Emit) error {
		if p.State != "rolling_back" {
			return ErrProposalState
		}
		p.State = "rolled_back"
		p.Verification = "object_removed"
		p.Detail = "newly created object removed; prior ANALYZE statistics cannot be restored"
		typ := "action_executed"
		if runErr != nil {
			p.State = "rollback_failed"
			p.Verification = "failed"
			p.Detail = runErr.Error()
			typ = "action_failed"
		}
		return emit(typ, map[string]any{"proposal_id": p.ID, "class": "revert_created_statistics", "state": p.State, "detail": p.Detail, "analyze_restored": false})
	})
	if saveErr != nil {
		return out, saveErr
	}
	return out, runErr
}
