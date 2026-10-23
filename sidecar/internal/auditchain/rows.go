package auditchain

import (
	"context"
	"fmt"
	"sort"
)

// liveRow is a row as it is now, with its sealed hash and state computed
// by the same expressions the trigger used.
type liveRow struct {
	chainSeq   *int64
	prev, hash *string
	sealed     string
	state      string
}

// latestLink is a row's last insert or state link anywhere in the chain.
type latestLink struct {
	seq   int64
	state string
}

// checkRows compares every row the window names with its links.
func (v *verifier) checkRows(ctx context.Context) error {
	ids := make([]int64, 0, len(v.rows))
	for id := range v.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	truncates, err := v.truncateSeqs(ctx)
	if err != nil {
		return err
	}
	for start := 0; start < len(ids); start += rowBatch {
		batch := ids[start:min(start+rowBatch, len(ids))]
		live, err := v.liveRows(ctx, batch)
		if err != nil {
			return err
		}
		latest, deleted, err := v.rowHistory(ctx, batch)
		if err != nil {
			return err
		}
		for _, id := range batch {
			v.checkRow(id, live[id], latest[id], deleted[id], truncates)
		}
	}
	return nil
}

func (v *verifier) checkRow(id int64, live *liveRow, latest *latestLink, deleted bool,
	truncates []int64) {
	links := v.rows[id]
	if live == nil {
		if deleted || links.deleted || truncatedAfter(truncates, firstSeq(links)) {
			return
		}
		v.rep.add(Problem{Kind: KindRowMissing, RowID: id, Seq: firstSeq(links),
			Detail: "the row is gone and no delete or truncate link records it"})
		return
	}
	v.rep.RowsChecked++
	for sealed, seq := range links.sealed {
		if sealed != live.sealed {
			v.rep.add(Problem{Kind: KindRowEdited, RowID: id, Seq: seq,
				Detail: "a sealed column differs from what the chain recorded"})
			break
		}
	}
	if len(v.s.State) > 0 && latest != nil && latest.state != live.state {
		v.rep.add(Problem{Kind: KindStateEdited, RowID: id, Seq: latest.seq,
			Detail: "the row's state differs from its last link (changed behind the chain)"})
	}
	if ins := links.insert; ins != nil && !rowCarries(live, ins) {
		v.rep.add(Problem{Kind: KindRowLinkMismatch, RowID: id, Seq: ins.Seq,
			Detail: "the row's chain columns do not match its insert link"})
	}
}

func rowCarries(r *liveRow, l *Link) bool {
	return r.chainSeq != nil && *r.chainSeq == l.Seq && r.prev != nil &&
		*r.prev == l.PrevHash && r.hash != nil && *r.hash == l.Hash
}

func firstSeq(r *rowLinks) int64 {
	first := int64(0)
	for _, seq := range r.sealed {
		if first == 0 || seq < first {
			first = seq
		}
	}
	return first
}

func truncatedAfter(truncates []int64, seq int64) bool {
	for _, t := range truncates {
		if t > seq {
			return true
		}
	}
	return false
}

func (v *verifier) liveRows(ctx context.Context, ids []int64) (map[int64]*liveRow, error) {
	sql := fmt.Sprintf(`/* pg_sage audit_chain v1 */ SELECT r.id, r.chain_seq,
		r.chain_prev_hash, r.chain_hash, %s, %s FROM %s r WHERE r.id = ANY($1::bigint[])`,
		v.s.sealedHashSQL("r"), v.s.stateSQL("r"), v.s.Table)
	rows, err := v.q.Query(ctx, sql, ids)
	if err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	defer rows.Close()
	out := map[int64]*liveRow{}
	for rows.Next() {
		var id int64
		r := &liveRow{}
		if err := rows.Scan(&id, &r.chainSeq, &r.prev, &r.hash, &r.sealed,
			&r.state); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return out, nil
}

// rowHistory reads each row's last insert-or-state link and whether a
// delete link exists, across the whole chain (a later link may lie outside
// the window).
func (v *verifier) rowHistory(ctx context.Context, ids []int64) (
	map[int64]*latestLink, map[int64]bool, error) {
	rows, err := v.q.Query(ctx, `/* pg_sage audit_chain v1 */
		SELECT DISTINCT ON (row_id) row_id, seq, op, state FROM sage.audit_chain_link
		WHERE chain = $1 AND row_id = ANY($2) AND op IN ('I', 'U', 'D')
		ORDER BY row_id, seq DESC`, v.s.Chain, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read row history: %w", err)
	}
	defer rows.Close()
	latest, deleted := map[int64]*latestLink{}, map[int64]bool{}
	for rows.Next() {
		var id, seq int64
		var op, state string
		if err := rows.Scan(&id, &seq, &op, &state); err != nil {
			return nil, nil, fmt.Errorf("scan row history: %w", err)
		}
		if op == "D" {
			deleted[id] = true
			continue
		}
		latest[id] = &latestLink{seq: seq, state: state}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read row history: %w", err)
	}
	return latest, deleted, nil
}

func (v *verifier) truncateSeqs(ctx context.Context) ([]int64, error) {
	rows, err := v.q.Query(ctx, `/* pg_sage audit_chain v1 */ SELECT seq
		FROM sage.audit_chain_link WHERE chain = $1 AND op = 'T' ORDER BY seq`, v.s.Chain)
	if err != nil {
		return nil, fmt.Errorf("read truncations: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("scan truncation: %w", err)
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}

// findUnchained reports rows newer than the install boundary that carry no
// link: written with the triggers disabled or bypassed. A full verification
// searches every such row; a window searches the id range its inserts span.
func (v *verifier) findUnchained(ctx context.Context, full bool) error {
	lo, hi := v.legacy+1, int64(1<<62)
	if !full {
		lo, hi = v.insertRange()
		if lo == 0 {
			return nil
		}
	}
	sql := fmt.Sprintf(`/* pg_sage audit_chain v1 */ SELECT id FROM %s
		WHERE id >= $1::bigint AND id <= $2::bigint AND id > $3::bigint
		AND chain_seq IS NULL
		ORDER BY id LIMIT $4`, v.s.Table)
	rows, err := v.q.Query(ctx, sql, lo, hi, v.legacy, MaxProblems+1)
	if err != nil {
		return fmt.Errorf("find unchained rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan unchained row: %w", err)
		}
		v.rep.add(Problem{Kind: KindUnchainedRow, RowID: id,
			Detail: "the row carries no link: it was written behind the chain"})
	}
	return rows.Err()
}

func (v *verifier) insertRange() (int64, int64) {
	var lo, hi int64
	for id, r := range v.rows {
		if r.insert == nil {
			continue
		}
		if lo == 0 || id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
	}
	return lo, hi
}

func (v *verifier) countLegacy(ctx context.Context) error {
	if v.legacy <= 0 {
		return nil
	}
	sql := fmt.Sprintf(`/* pg_sage audit_chain v1 */ SELECT count(*) FROM %s
		WHERE id <= $1::bigint`, v.s.Table)
	if err := v.q.QueryRow(ctx, sql, v.legacy).Scan(&v.rep.LegacyRows); err != nil {
		return fmt.Errorf("count legacy rows: %w", err)
	}
	return nil
}
