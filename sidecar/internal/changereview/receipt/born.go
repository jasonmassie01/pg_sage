package receipt

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Born queryids: statements first seen after landing on a touched
// relation, the ones the review could not watch.
const (
	// BornLookback is how far back a queryid must be absent from
	// query_store to count as first seen after landing.
	BornLookback = 7 * 24 * time.Hour
	// MaxBorn bounds the born queryids one receipt lists.
	MaxBorn = 20
	// maxBornCandidates bounds the new queryids read before the relation
	// filter.
	maxBornCandidates = 200
)

// bornSQL lists queryids sampled in the after window with no sample in
// the lookback before landing, with their newest cumulative counters. It
// reads idx_query_store_time for the window and idx_query_store_qid_time
// per candidate.
const bornSQL = `/* pg_sage change_review_receipt_born v1 */
SELECT q.queryid, min(q.captured_at),
	(array_agg(q.calls ORDER BY q.captured_at DESC))[1],
	(array_agg(q.total_exec_time ORDER BY q.captured_at DESC))[1]
FROM sage.query_store q
WHERE {db:q} AND q.captured_at >= $1 AND q.captured_at <= $2
  AND NOT EXISTS (SELECT 1 FROM sage.query_store p
                  WHERE {db:p} AND p.queryid = q.queryid
                    AND p.captured_at < $1 AND p.captured_at >= $3)
GROUP BY q.queryid
ORDER BY min(q.captured_at), q.queryid
LIMIT $4`

// bornTextSQL reads the candidates' statement text in this database.
const bornTextSQL = `/* pg_sage change_review_receipt_born_text v1 */
SELECT s.queryid, s.query FROM pg_stat_statements s
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND s.queryid = ANY($1::bigint[])`

// born lists the born queryids of r: first seen in [landed_at, after_to],
// reading a touched relation (by their pg_stat_statements text).
func (o Observer) born(ctx context.Context, r changereview.Review,
	w Windows) ([]changereview.BornQuery, error) {
	rels := r.Expectations.Relations
	if len(rels) == 0 {
		return nil, nil
	}
	cands, err := o.bornCandidates(ctx, w)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	texts, err := o.statementTexts(ctx, cands)
	if err != nil {
		return nil, err
	}
	watched := map[int64]bool{}
	for _, id := range r.Expectations.WatchQueryIDs {
		watched[id] = true
	}
	var out []changereview.BornQuery
	for _, c := range cands {
		text, ok := texts[c.QueryID]
		if !ok || watched[c.QueryID] {
			continue
		}
		if c.Relations = touched(text, rels); len(c.Relations) > 0 {
			out = append(out, c)
		}
		if len(out) == MaxBorn {
			break
		}
	}
	return out, nil
}

func (o Observer) bornCandidates(ctx context.Context, w Windows) ([]changereview.BornQuery,
	error) {
	rows, err := histstore.Resolve(o.Pool).Query(ctx, bornSQL, w.AfterFrom, w.AfterTo,
		w.AfterFrom.Add(-BornLookback), maxBornCandidates)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []changereview.BornQuery
	for rows.Next() {
		var b changereview.BornQuery
		var total float64
		if err := rows.Scan(&b.QueryID, &b.FirstSeen, &b.Calls, &total); err != nil {
			return nil, err
		}
		b.FirstSeen = b.FirstSeen.UTC()
		if b.Calls > 0 {
			b.MeanMs = total / float64(b.Calls)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (o Observer) statementTexts(ctx context.Context,
	cands []changereview.BornQuery) (map[int64]string, error) {
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.QueryID)
	}
	rows, err := o.Pool.Query(ctx, bornTextSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		if _, dup := out[id]; !dup {
			out[id] = text
		}
	}
	return out, rows.Err()
}

// touched are the relations of rels ("schema.name") that text reads: by
// its parse tree (an unqualified name matches any schema), or, without
// the parser or when it rejects the text, by whole-word name match.
func touched(text string, rels []string) []string {
	refs, err := sqlast.References(text)
	var out []string
	for _, rel := range rels {
		schema, name, ok := strings.Cut(rel, ".")
		if !ok {
			continue
		}
		if err == nil && refsName(refs, schema, name) ||
			err != nil && namesWord(text, name) {
			out = append(out, rel)
		}
	}
	return out
}

func refsName(refs sqlast.Refs, schema, name string) bool {
	for _, r := range refs.Relations {
		if r.Name == name && (r.Schema == "" || r.Schema == schema) {
			return true
		}
	}
	return false
}

// namesWord reports whether text names name as a whole identifier word.
func namesWord(text, name string) bool {
	re, err := regexp.Compile(`(?i)(^|[^a-z0-9_$])` + regexp.QuoteMeta(name) +
		`([^a-z0-9_$]|$)`)
	return err == nil && re.MatchString(text)
}
