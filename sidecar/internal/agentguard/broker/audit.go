package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
)

// AuditRecord is one sage.guard_query_audit row: one per agent_query call
// (§6.8), with the gate's reason and step.
type AuditRecord struct {
	PrincipalID  string
	TaskID       string
	EnvelopeHash string
	Verdict      string
	// Reason is the refusal reason, "sqlstate_<code>" for a failed
	// execution, "" for a successful one.
	Reason      string
	Step        string
	RowCount    *int
	Classes     []string
	Fingerprint string
	BrokerRole  string
}

// AuditSink records audit rows on the target database.
type AuditSink interface {
	Record(ctx context.Context, t Target, rec AuditRecord) error
}

// SQLAudit writes sage.guard_query_audit on the target with pg_sage's own
// pool, with the pg_stat_statements snapshot the attribution view
// compares against (NULL when the extension is not installed).
type SQLAudit struct{}

// Record inserts one row.
func (SQLAudit) Record(ctx context.Context, t Target, rec AuditRecord) error {
	if t.Pool == nil {
		return fmt.Errorf("%w: no pool for %s", ErrUnavailable, t.Name)
	}
	snap := readPSSInfo(ctx, t.Pool)
	var dealloc *int64
	var reset *time.Time
	if snap != nil {
		dealloc, reset = &snap.Dealloc, &snap.Reset
	}
	classes := rec.Classes
	if classes == nil {
		classes = []string{}
	}
	_, err := t.Pool.Exec(ctx, `/* pg_sage guard_query_audit v1 */
		INSERT INTO sage.guard_query_audit (database_id, principal_id, task_id,
			envelope_hash, verdict, reason, step, row_count, classes, fingerprint,
			broker_role, pss_dealloc, pss_reset)
		VALUES ($1::uuid, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8,
			$9, NULLIF($10, ''), NULLIF($11, ''), $12, $13)`,
		t.DatabaseID, rec.PrincipalID, rec.TaskID, rec.EnvelopeHash, rec.Verdict, rec.Reason,
		rec.Step, rec.RowCount, classes, rec.Fingerprint, rec.BrokerRole, dealloc, reset)
	if err != nil {
		return fmt.Errorf("%w: recording the agent_query audit row: %v", ErrUnavailable, err)
	}
	return nil
}

// pssInfo is pg_stat_statements_info: evictions and the last full reset.
type pssInfo struct {
	Dealloc int64
	Reset   time.Time
}

// readPSSInfo returns nil when pg_stat_statements is not installed or not
// readable; attribution then falls back to the audit.
func readPSSInfo(ctx context.Context, pool *pgxpool.Pool) *pssInfo {
	var info pssInfo
	err := pool.QueryRow(ctx, `/* pg_sage guard_attribution v1 */
		SELECT dealloc, stats_reset FROM pg_stat_statements_info`).
		Scan(&info.Dealloc, &info.Reset)
	if err != nil {
		return nil
	}
	return &info
}

// StoreClasses reads column classes from the target's sage.facts
// (classify.Store) with pg_sage's own pool.
type StoreClasses struct{}

// Lookup returns the bound classes of one relation.
func (StoreClasses) Lookup(ctx context.Context, t Target, relid uint32) (
	classify.RelationClasses, error) {
	return classify.NewStore(t.Pool).Lookup(ctx, relid)
}
