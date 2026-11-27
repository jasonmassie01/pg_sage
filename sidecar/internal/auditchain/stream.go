package auditchain

import (
	"context"
	"fmt"
	"math"
)

// streamLink is one row link of the ordered row pass, with its row as the
// same statement saw it (live is nil when the row is gone).
type streamLink struct {
	Link
	live *liveRow
}

// streamRow gathers one row's links while the pass is on it.
type streamRow struct {
	id      int64
	links   rowLinks
	unknown bool
	live    *liveRow
}

// checkAllRows checks every chained row of a full verification from an
// ordered pass over the link index ((row_id, seq), keyset batches of
// linkBatch links). Each batch reads its links and their rows in one
// statement, so a row and its links come from one snapshot; only the row
// in progress is held, so deleted rows and finished rows cost nothing.
// A row whose links span batches is finished with the rows of the batch
// holding its last link: links are append-only, so that is consistent.
func (v *verifier) checkAllRows(ctx context.Context) error {
	sql := v.rowPassSQL()
	keyRow, keySeq := int64(math.MinInt64), int64(math.MinInt64)
	var cur *streamRow
	var truncates []int64
	for {
		batch, err := v.rowPassBatch(ctx, sql, keyRow, keySeq)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		if truncates, err = v.truncateSeqs(ctx); err != nil {
			return err
		}
		for i := range batch {
			cur = v.feed(cur, &batch[i], truncates)
		}
		last := batch[len(batch)-1]
		keyRow, keySeq = last.RowID, last.Seq
		if len(batch) < linkBatch {
			break
		}
	}
	if cur != nil {
		v.finish(cur, truncates)
	}
	return nil
}

// feed adds one link to the row in progress, finishing the previous row
// when the link starts another.
func (v *verifier) feed(cur *streamRow, l *streamLink, truncates []int64) *streamRow {
	if cur != nil && cur.id != l.RowID {
		v.finish(cur, truncates)
		cur = nil
	}
	if cur == nil {
		cur = &streamRow{id: l.RowID, links: rowLinks{sealed: map[string]int64{}}}
	}
	cur.live = l.live
	if l.V != v.s.Version {
		cur.unknown = true // the walk reported the link; the row is not checked
		return cur
	}
	cur.links.note(&l.Link)
	return cur
}

func (v *verifier) finish(r *streamRow, truncates []int64) {
	if r.unknown {
		return
	}
	v.checkRow(r.id, &r.links, r.live, r.links.latest, false, truncates)
}

// rowPassSQL reads the next linkBatch row links after ($2, $3) in (row_id,
// seq) order, joined with their rows' current content.
func (v *verifier) rowPassSQL() string {
	return fmt.Sprintf(`/* pg_sage audit_chain v1 */
WITH l AS MATERIALIZED (
    SELECT row_id, seq, op, v, sealed_hash, state, prev_hash, hash
    FROM sage.audit_chain_link
    WHERE chain = $1 AND (chain, row_id, seq) > ($1, $2::bigint, $3::bigint)
      AND op NOT IN ('T', 'C', 'P')
    ORDER BY chain, row_id, seq LIMIT $4),
r AS MATERIALIZED (
    SELECT t.id, t.chain_seq, t.chain_prev_hash, t.chain_hash, %s AS sealed,
        %s AS state
    FROM %s t WHERE t.id IN (SELECT row_id FROM l))
SELECT l.row_id, l.seq, l.op, l.v, l.sealed_hash, l.state, l.prev_hash, l.hash,
    r.id IS NOT NULL, r.chain_seq, r.chain_prev_hash, r.chain_hash, r.sealed, r.state
FROM l LEFT JOIN r ON r.id = l.row_id ORDER BY l.row_id, l.seq`,
		v.s.sealedHashSQL("t"), v.s.stateSQL("t"), v.s.Table)
}

func (v *verifier) rowPassBatch(ctx context.Context, sql string, keyRow, keySeq int64) (
	[]streamLink, error) {
	rows, err := v.q.Query(ctx, sql, v.s.Chain, keyRow, keySeq, linkBatch)
	if err != nil {
		return nil, fmt.Errorf("read row links after (%d, %d): %w", keyRow, keySeq, err)
	}
	defer rows.Close()
	out := make([]streamLink, 0, linkBatch)
	for rows.Next() {
		var l streamLink
		var present bool
		live := &liveRow{}
		var sealed, state *string
		if err := rows.Scan(&l.RowID, &l.Seq, &l.Op, &l.V, &l.SealedHash, &l.State,
			&l.PrevHash, &l.Hash, &present, &live.chainSeq, &live.prev, &live.hash,
			&sealed, &state); err != nil {
			return nil, fmt.Errorf("scan row link: %w", err)
		}
		if present {
			live.sealed, live.state = deref(sealed), deref(state)
			l.live = live
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read row links after (%d, %d): %w", keyRow, keySeq, err)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
