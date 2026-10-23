package siem

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// readRecords reads up to limit links of chain after position after, each
// with the current content of its row (chain columns removed). Reading by
// the link table's primary key keeps every pass an index range scan.
func readRecords(ctx context.Context, src Source, chain string, after int64,
	limit int) ([]Record, error) {
	rowExpr := "NULL::jsonb"
	if spec, ok := auditchain.SpecFor(chain); ok {
		rowExpr = fmt.Sprintf(`(SELECT to_jsonb(t) - 'chain_seq' - 'chain_prev_hash'
			- 'chain_hash' FROM %s t WHERE t.id = l.row_id)`, spec.Table)
	}
	sql := fmt.Sprintf(`/* pg_sage siem v1 */ SELECT l.seq, l.row_id, l.op, l.v,
		l.sealed_hash, l.state, l.prev_hash, l.hash, l.at, l.db_user, l.app, %s
		FROM sage.audit_chain_link l WHERE l.chain = $1 AND l.seq > $2
		ORDER BY l.seq LIMIT $3`, rowExpr)
	rows, err := src.DB.Query(ctx, sql, chain, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read %s links of %s: %w", chain, src.Name, err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r := Record{Source: src.Name, Chain: chain}
		l := &r.Link
		if err := rows.Scan(&l.Seq, &l.RowID, &l.Op, &l.V, &l.SealedHash, &l.State,
			&l.PrevHash, &l.Hash, &l.At, &l.DBUser, &l.App, &r.Row); err != nil {
			return nil, fmt.Errorf("scan %s link of %s: %w", chain, src.Name, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s links of %s: %w", chain, src.Name, err)
	}
	return out, nil
}
