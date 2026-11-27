package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/sre"
)

// maintenanceStrandedMargin pads the longest a live typed run can hold its
// proposal before the sweeper may decide the run died.
const maintenanceStrandedMargin = time.Minute

// errSettled reports a stranded proposal another writer settled first.
var errSettled = errors.New("maintenance proposal already settled")

// strandedAfter is how long a proposal may stay executing or rolling_back
// before the sweeper treats its run as dead. A live run holds the proposal
// for at most one apply timeout waiting for a DDL slot (RunApproved and
// Rollback bound that wait) and one more for the executor's detached run.
func (m *MaintenanceService) strandedAfter() time.Duration {
	return 2*m.Executor.ApplyTimeout() + maintenanceStrandedMargin
}

// strandedCutoff is the last updated_at a stranded proposal can have. It is
// measured on the clock that stamps updated_at unless a test injects Now.
func (m *MaintenanceService) strandedCutoff(ctx context.Context) (time.Time, error) {
	if m.Now != nil {
		return m.Now().Add(-m.strandedAfter()), nil
	}
	var now time.Time
	if err := m.st.Pool().QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return now.Add(-m.strandedAfter()), nil
}

// Reconcile settles maintenance runs that ended without recording their
// outcome: a crash or a control-database failure between the claim and the
// record. It reads the bound table's statistics catalog under that table's
// lock and the exact action record; it never re-runs the forward SQL or the
// rollback.
func (m *MaintenanceService) Reconcile(ctx context.Context) error {
	scope, err := m.scope(ctx)
	if err != nil {
		return fmt.Errorf("maintenance reconcile: %w", err)
	}
	cutoff, err := m.strandedCutoff(ctx)
	if err != nil {
		return fmt.Errorf("maintenance reconcile: %w", err)
	}
	rows, err := m.st.Pool().Query(ctx, `SELECT id::text FROM sage.sre_maintenance_proposals
		WHERE deployment_id=$1 AND database_id=$2 AND state IN ('executing','rolling_back')
		  AND updated_at <= $3 ORDER BY updated_at LIMIT 10`,
		string(scope.DeploymentID), string(scope.DatabaseID), cutoff)
	if err != nil {
		return fmt.Errorf("maintenance reconcile: list stranded proposals: %w", err)
	}
	ids, err := textColumn(rows)
	if err != nil {
		return fmt.Errorf("maintenance reconcile: list stranded proposals: %w", err)
	}
	var errs []error
	for _, id := range ids {
		err := m.reconcileOne(ctx, sre.UUID(id), cutoff)
		if err != nil && !errors.Is(err, errSettled) {
			errs = append(errs, fmt.Errorf("maintenance reconcile %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileOne settles one stranded proposal in a single transaction: the
// observation, the proposal, the queue or action ledger and the event commit
// together or not at all.
func (m *MaintenanceService) reconcileOne(ctx context.Context, id sre.UUID,
	cutoff time.Time) error {
	_, err := m.mutateTx(ctx, id, systemActor,
		func(tx pgx.Tx, p *MaintenanceProposal, emit sre.Emit) error {
			if err := stillStranded(ctx, tx, id, cutoff); err != nil {
				return err
			}
			obs, err := observeStatistics(ctx, tx, *p)
			if err != nil {
				return err
			}
			from := p.State
			typ := settleStranded(p, obs)
			if typ == "" {
				return errSettled
			}
			if err := settleLedger(ctx, tx, from, *p); err != nil {
				return err
			}
			return emit(typ, reconcilePayload(*p, from))
		})
	return err
}

// stillStranded re-checks, under the row lock, that no writer touched the
// proposal since the sweeper listed it.
func stillStranded(ctx context.Context, tx pgx.Tx, id sre.UUID, cutoff time.Time) error {
	var stale bool
	err := tx.QueryRow(ctx, `SELECT updated_at <= $2 AND state IN ('executing','rolling_back')
		FROM sage.sre_maintenance_proposals WHERE id=$1`, string(id), cutoff).Scan(&stale)
	if err != nil {
		return fmt.Errorf("re-read stranded proposal: %w", err)
	}
	if !stale {
		return errSettled
	}
	return nil
}

// statisticsObservation is what the catalog and the action ledger show for
// one stranded proposal, read under the bound table's lock.
type statisticsObservation struct {
	actions   int    // action_log rows the maintenance link matches
	actionID  int64  // the matching row when actions == 1
	outcome   string // its outcome
	objectOID uint32 // the proposal's statistics name on the bound table, else 0
	foreign   bool   // that name is held by an object on another table
}

// observeStatistics takes the lock the executor's guard takes on the bound
// table, so an in-flight CREATE or DROP finishes first, then reads the
// statistics object by its deterministic name and, for a forward run, the
// exact action record.
func observeStatistics(ctx context.Context, tx pgx.Tx,
	p MaintenanceProposal) (statisticsObservation, error) {
	var o statisticsObservation
	c, err := extstats.ParseCreate(p.SQL)
	if err != nil {
		return o, fmt.Errorf("parse bound statistics SQL: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout','2000',true),
		set_config('statement_timeout','10000',true)`); err != nil {
		return o, fmt.Errorf("bound reconciliation timeouts: %w", err)
	}
	if err = lockBoundTable(ctx, tx, p.Target.TableOID); err != nil {
		return o, err
	}
	var oid, relid uint32
	err = tx.QueryRow(ctx, `SELECT s.oid, s.stxrelid FROM pg_catalog.pg_statistic_ext s
		JOIN pg_catalog.pg_namespace n ON n.oid=s.stxnamespace
		WHERE n.nspname=$1 AND s.stxname=$2`, c.Schema.Name, c.Name.Name).Scan(&oid, &relid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return o, fmt.Errorf("read statistics catalog: %w", err)
	case relid == p.Target.TableOID:
		o.objectOID = oid
	default:
		o.foreign = true
	}
	if p.State != "executing" {
		return o, nil
	}
	return o, readMaintenanceActions(ctx, tx, p.ID, &o)
}

// readMaintenanceActions finds the action records the retention link ties to
// this proposal; more than one is reported, never chosen between.
func readMaintenanceActions(ctx context.Context, tx pgx.Tx, id sre.UUID,
	o *statisticsObservation) error {
	rows, err := tx.Query(ctx, `SELECT al.id, al.outcome FROM sage.action_log al
		JOIN sage.sre_maintenance_proposals p ON p.id = $1
		WHERE `+sre.MaintenanceActionLink+` ORDER BY al.id LIMIT 3`, string(id))
	if err != nil {
		return fmt.Errorf("read maintenance action record: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		o.actions++
		if err := rows.Scan(&o.actionID, &o.outcome); err != nil {
			return fmt.Errorf("read maintenance action record: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read maintenance action record: %w", err)
	}
	if o.actions != 1 {
		o.actionID, o.outcome = 0, ""
	}
	return nil
}

// lockBoundTable locks the bound table by its OID's current name. A dropped
// table has no statistics left, so there is nothing to wait for.
func lockBoundTable(ctx context.Context, tx pgx.Tx, tableOID uint32) error {
	name, err := boundTableName(ctx, tx, tableOID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve bound table %d: %w", tableOID, err)
	}
	// name is quoted by format('%I.%I') from the catalog, never user input.
	if _, err = tx.Exec(ctx, "LOCK TABLE "+name+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock bound table %s: %w", name, err)
	}
	again, err := boundTableName(ctx, tx, tableOID)
	if err != nil {
		return fmt.Errorf("re-resolve bound table %d: %w", tableOID, err)
	}
	if again != name {
		return fmt.Errorf("bound table %d was renamed from %s while locking", tableOID, name)
	}
	return nil
}

func boundTableName(ctx context.Context, tx pgx.Tx, tableOID uint32) (string, error) {
	var name string
	err := tx.QueryRow(ctx, `SELECT pg_catalog.format('%I.%I', n.nspname, c.relname)
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=$1 AND c.relkind='r'`, tableOID).Scan(&name)
	return name, err
}

// settleLedger keeps the approval item and the executor's action record in
// step with the settled proposal, in the same transaction.
func settleLedger(ctx context.Context, tx pgx.Tx, from string, p MaintenanceProposal) error {
	if from == "executing" {
		_, err := tx.Exec(ctx, `UPDATE sage.action_queue SET verification_status=$2,
			action_log_id=NULLIF($3,0) WHERE id=$1`, p.QueueID, p.Verification, p.ActionLogID)
		if err != nil {
			return fmt.Errorf("record reconciled queue verification: %w", err)
		}
		return nil
	}
	// A rollback claimed but never finished leaves the action rolling_back,
	// which would refuse the retry; settle it the way a failed DROP does.
	_, err := tx.Exec(ctx, `UPDATE sage.action_log SET outcome=$2, rollback_reason=$3,
		measured_at=now() WHERE id=$1 AND outcome='rolling_back'`,
		p.ActionLogID, p.State, p.Detail)
	if err != nil {
		return fmt.Errorf("record reconciled rollback outcome: %w", err)
	}
	return nil
}

func reconcilePayload(p MaintenanceProposal, from string) map[string]any {
	class := p.Class
	if from == "rolling_back" {
		class = "revert_created_statistics"
	}
	return map[string]any{"proposal_id": p.ID, "class": class, "outcome": p.State,
		"state": p.State, "action_log_id": p.ActionLogID, "statistics_oid": p.StatisticsOID,
		"structural_verification": p.Verification, "detail": p.Detail,
		"reconciled": true, "forward_sql_rerun": false}
}
