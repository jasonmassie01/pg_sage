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
	// maxBornCandidates bounds the statements on the touched relations
	// whose history is probed (the busiest first).
	maxBornCandidates = 200
)

// bornTextSQL is the statements of this database whose text names a
// touched relation as a whole word, busiest first: the candidates.
const bornTextSQL = `/* pg_sage change_review_receipt_born_text v2 */
SELECT s.queryid, min(s.query) FROM pg_stat_statements s
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND s.queryid IS NOT NULL AND s.query ~* $1
GROUP BY s.queryid ORDER BY sum(s.calls) DESC, 1 LIMIT $2`

// bornSQL keeps the candidates sampled in the after window with no sample
// in the lookback before landing, with their first sample and newest
// cumulative counters there. Each test is one index probe per candidate
// (idx_query_store_qid_time): it never reads the window's samples.
const bornSQL = `/* pg_sage change_review_receipt_born v2 */
SELECT c.queryid, f.first, l.calls, l.total
FROM unnest($1::int8[]) AS c(queryid)
CROSS JOIN LATERAL (
  SELECT q.captured_at AS first FROM sage.query_store q
   WHERE {db:q} AND q.queryid = c.queryid AND q.captured_at >= $2 AND q.captured_at <= $3
   ORDER BY q.captured_at LIMIT 1) f
CROSS JOIN LATERAL (
  SELECT q.calls, q.total_exec_time AS total FROM sage.query_store q
   WHERE {db:q} AND q.queryid = c.queryid AND q.captured_at >= $2 AND q.captured_at <= $3
   ORDER BY q.captured_at DESC LIMIT 1) l
WHERE NOT EXISTS (SELECT 1 FROM sage.query_store p
                  WHERE {db:p} AND p.queryid = c.queryid
                    AND p.captured_at < $2 AND p.captured_at >= $4)
ORDER BY f.first, c.queryid
LIMIT $5`

// born lists the born queryids of r: statements on a touched relation
// (by their pg_stat_statements text) first seen in [landed_at, after_to].
func (o Observer) born(ctx context.Context, r changereview.Review,
	w Windows) ([]changereview.BornQuery, error) {
	rels := r.Expectations.Relations
	if len(rels) == 0 {
		return nil, nil
	}
	texts, ids, err := o.candidateTexts(ctx, rels, r.Expectations.WatchQueryIDs)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	cands, err := o.bornCandidates(ctx, ids, w)
	if err != nil {
		return nil, err
	}
	var out []changereview.BornQuery
	for _, c := range cands {
		if c.Relations = touched(texts[c.QueryID], rels); len(c.Relations) > 0 {
			out = append(out, c)
		}
		if len(out) == MaxBorn {
			break
		}
	}
	return out, nil
}

// candidateTexts reads the statements naming a touched relation that the
// review did not watch.
func (o Observer) candidateTexts(ctx context.Context, rels []string, watch []int64) (
	map[int64]string, []int64, error) {
	rctx, cancel := o.readContext(ctx)
	defer cancel()
	rows, err := o.Pool.Query(rctx, bornTextSQL, relationWords(rels), maxBornCandidates)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	watched := map[int64]bool{}
	for _, id := range watch {
		watched[id] = true
	}
	texts := map[int64]string{}
	var ids []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, nil, err
		}
		if !watched[id] {
			texts[id] = text
			ids = append(ids, id)
		}
	}
	return texts, ids, rows.Err()
}

func (o Observer) bornCandidates(ctx context.Context, ids []int64,
	w Windows) ([]changereview.BornQuery, error) {
	rctx, cancel := o.readContext(ctx)
	defer cancel()
	rows, err := histstore.Resolve(o.Pool).Query(rctx, bornSQL, ids, w.AfterFrom, w.AfterTo,
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

var regexMeta = regexp.MustCompile(`[.^$|?*+()\[\]{}\\]`)

// relationWords matches the bare names of rels ("schema.name") as whole
// words (PostgreSQL regex).
func relationWords(rels []string) string {
	words := make([]string, 0, len(rels))
	for _, rel := range rels {
		name := rel
		if _, n, ok := strings.Cut(rel, "."); ok {
			name = n
		}
		words = append(words, regexMeta.ReplaceAllString(name, `\$0`))
	}
	return `\m(` + strings.Join(words, "|") + `)\M`
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
