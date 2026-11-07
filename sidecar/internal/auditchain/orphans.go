package auditchain

import (
	"context"
	"fmt"
)

// findOrphans reports chained rows whose own link (the insert link their
// chain columns name) is no longer in the chain: links removed from the
// tail, a whole chain deleted, or positions reused after either. The walk
// cannot see these, since it trusts the head it reads from the same table.
// It runs on a full verification only, as one bounded query. Rows whose
// insert link the walk saw are skipped: checkRows already compared them.
func (v *verifier) findOrphans(ctx context.Context) error {
	sql := fmt.Sprintf(`/* pg_sage audit_chain v1 */ SELECT r.id, r.chain_seq FROM %s r
		WHERE r.chain_seq IS NOT NULL AND NOT EXISTS (SELECT 1
			FROM sage.audit_chain_link l WHERE l.chain = $1 AND l.seq = r.chain_seq
			AND l.op = 'I' AND l.row_id = r.id AND l.hash = r.chain_hash)
		ORDER BY r.id LIMIT $2`, v.s.Table)
	rows, err := v.q.Query(ctx, sql, v.s.Chain, MaxProblems+1)
	if err != nil {
		return fmt.Errorf("find orphan rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			return fmt.Errorf("scan orphan row: %w", err)
		}
		if r := v.rows[id]; r != nil && r.insert != nil {
			continue
		}
		v.rep.add(Problem{Kind: KindOrphanRow, RowID: id, Seq: seq,
			Detail: fmt.Sprintf("the row carries link %d, which the chain no longer "+
				"holds (links removed from the tail, or the chain deleted)", seq)})
	}
	return rows.Err()
}
