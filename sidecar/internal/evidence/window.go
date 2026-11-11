package evidence

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// windowFile is one file of a window pack: its table and time column, the
// chain that proves it and whether the control database holds
// it (sign-ins and config changes are written there in fleet mode).
type windowFile struct {
	name    string
	table   string
	timeCol string
	spec    *auditchain.Spec
	control bool
}

// windowFiles are the files of a window pack, in chains.json order.
var windowFiles = []windowFile{
	{"actions.json", auditchain.ActionLog.Table, "executed_at", &auditchain.ActionLog, false},
	{"auth_audit.json", auditchain.AuthAudit.Table, "created_at", &auditchain.AuthAudit, true},
	{"config_audit.json", auditchain.ConfigAudit.Table, "changed_at",
		&auditchain.ConfigAudit, true},
	{"pgaudit.json", auditchain.PGAuditEvents.Table, "logged_at",
		&auditchain.PGAuditEvents, false},
	{"guard_query_audit.json", auditchain.GuardQueryAudit.Table, "at",
		&auditchain.GuardQueryAudit, false},
	{"decisions.json", auditchain.DecisionLedger.Table, "created_at",
		&auditchain.DecisionLedger, false},
	{"policy_approvals.json", auditchain.PolicyApprovals.Table, "decided_at",
		&auditchain.PolicyApprovals, true},
}

// noDatabaseIDNote marks a fleet config_audit.json read without the
// database's id: it holds the global changes only.
const noDatabaseIDNote = "the database's id is unknown: only global config changes " +
	"(database_id NULL) are included"

// noDatabaseIDApprovalsNote is noDatabaseIDNote for policy_approvals.json.
const noDatabaseIDApprovalsNote = "the database's id is unknown: only approvals of " +
	"global policy versions (database_id NULL) are included"

// approvalsOfDatabase selects the approvals of the policy versions of
// database $3 and the global ones.
const approvalsOfDatabase = " AND t.policy_id IN (SELECT p.id FROM sage.policy p " +
	"WHERE p.database_id = $3 OR p.database_id IS NULL)"

// approvalsGlobal selects the approvals of the global policy versions.
const approvalsGlobal = " AND t.policy_id IN (SELECT p.id FROM sage.policy p " +
	"WHERE p.database_id IS NULL)"

// window collects the audit of [from, to): the rows of every audit table
// and decision in it, and each chain verified over the positions of the
// rows taken. A continuation collects its one file only.
func (b *builder) window(ctx context.Context, from, to time.Time) error {
	proofs := []chainProof{}
	for _, f := range windowFiles {
		if b.scope.File != "" && f.name != b.scope.File {
			continue
		}
		q := b.windowQuery(f, from, to)
		meta, exists, err := b.rows(ctx, q)
		if err != nil {
			return err
		}
		proof, err := b.windowChain(ctx, *f.spec, q, meta, exists)
		if err != nil {
			return err
		}
		proofs = append(proofs, proof)
	}
	return b.add("chains.json", fileMeta{}, proofs)
}

// windowQuery selects f's rows in [from, to), above the continuation's id.
// In fleet mode config changes are the database's own and the global ones.
func (b *builder) windowQuery(f windowFile, from, to time.Time) rowQuery {
	q := rowQuery{name: f.name, table: f.table, src: b.db, args: []any{from, to},
		where: fmt.Sprintf("t.%s >= $1 AND t.%s < $2", f.timeCol, f.timeCol)}
	if f.control {
		q.src = b.control
	}
	if b.fleet() {
		b.fleetFilter(f, &q)
	}
	if b.scope.AfterID > 0 {
		q.args = append(q.args, b.scope.AfterID)
		q.where += fmt.Sprintf(" AND t.id > $%d", len(q.args))
	}
	return q
}

// fleetFilter narrows a control database file to the requested database's
// rows and the global ones: its config changes, and the approvals of its
// policy versions.
func (b *builder) fleetFilter(f windowFile, q *rowQuery) {
	switch {
	case f.spec == &auditchain.ConfigAudit && b.databaseID > 0:
		q.args = append(q.args, b.databaseID)
		q.where += " AND (t.database_id = $3 OR t.database_id IS NULL)"
	case f.spec == &auditchain.ConfigAudit:
		q.where += " AND t.database_id IS NULL"
		q.note = noDatabaseIDNote
	case f.spec == &auditchain.PolicyApprovals && b.databaseID > 0:
		q.args = append(q.args, b.databaseID)
		q.where += approvalsOfDatabase
	case f.spec == &auditchain.PolicyApprovals:
		q.where += approvalsGlobal
		q.note = noDatabaseIDApprovalsNote
	}
}

// windowChain verifies a chain, in the database the file was read from,
// over the positions of the rows the file took.
func (b *builder) windowChain(ctx context.Context, spec auditchain.Spec, q rowQuery,
	meta fileMeta, exists bool) (chainProof, error) {
	proof := chainProof{Chain: spec.Chain, Source: q.src.name}
	if !exists {
		proof.Note = "table not installed"
		return proof, nil
	}
	if meta.lastID == nil {
		proof.Note = "no chained rows in the window"
		return proof, b.head(ctx, q.src, spec.Chain, &proof)
	}
	args := append(append([]any{}, q.args...), *meta.lastID)
	sql := fmt.Sprintf(`/* pg_sage evidence v1 */ SELECT min(t.chain_seq),
		max(t.chain_seq) FROM %s t WHERE (%s) AND t.id <= $%d`, spec.Table, q.where,
		len(args))
	var lo, hi *int64
	if err := q.src.q.QueryRow(ctx, sql, args...).Scan(&lo, &hi); err != nil {
		return proof, fmt.Errorf("evidence: %s positions in %s: %w", spec.Chain,
			q.src.describe(), err)
	}
	if lo == nil || hi == nil {
		proof.Note = "no chained rows in the window"
	} else if err := b.verify(ctx, q.src, spec, auditchain.Window{FromSeq: *lo,
		ToSeq: *hi}, &proof); err != nil {
		return proof, err
	}
	return proof, b.head(ctx, q.src, spec.Chain, &proof)
}
