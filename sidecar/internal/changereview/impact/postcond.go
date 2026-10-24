package impact

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
)

// Post-condition states.
const (
	PostLanded    = "landed"
	PostNotLanded = "not_landed"
	PostInvalid   = "invalid"
	PostUnknown   = "unknown"
)

// PostResult is one post-condition evaluated against the catalog: State
// is PostLanded, PostNotLanded, PostInvalid (an index left invalid by a
// failed CONCURRENTLY build) or PostUnknown (the relation of a column
// condition does not exist).
type PostResult struct {
	Condition changereview.PostCondition `json:"condition"`
	State     string                     `json:"state"`
}

// postConditionsSQL evaluates every post-condition in one catalog
// statement; rows come back in input order.
const postConditionsSQL = `/* pg_sage change_review_postconditions v1 */
SELECT u.ord::int, CASE
  WHEN u.kind = 'index_valid' THEN coalesce((
       SELECT CASE WHEN i.indisvalid AND i.indisready THEN 'landed' ELSE 'invalid' END
         FROM pg_catalog.pg_class ci JOIN pg_catalog.pg_index i ON i.indexrelid = ci.oid
        WHERE ci.relnamespace = ns.oid AND ci.relname = u.idx), 'not_landed')
  WHEN u.kind = 'index_absent' THEN CASE WHEN EXISTS (
       SELECT 1 FROM pg_catalog.pg_class ci
        WHERE ci.relnamespace = ns.oid AND ci.relname = u.idx AND ci.relkind IN ('i', 'I'))
       THEN 'not_landed' ELSE 'landed' END
  WHEN u.kind IN ('relation_present', 'relation_absent', 'relation_renamed') THEN CASE
       WHEN u.kind = 'relation_present' THEN CASE WHEN r.oid IS NULL THEN 'not_landed'
            ELSE 'landed' END
       WHEN r.oid IS NOT NULL THEN 'not_landed'
       WHEN u.kind = 'relation_absent' THEN 'landed'
       WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_class c
             WHERE c.relnamespace = ns.oid AND c.relname = u.newname) THEN 'landed'
       ELSE 'not_landed' END
  WHEN r.oid IS NULL THEN 'unknown'
  WHEN u.kind = 'constraint_present' THEN CASE WHEN EXISTS (
       SELECT 1 FROM pg_catalog.pg_constraint k
        WHERE k.conrelid = r.oid AND k.conname = u.con
          AND (u.typ <> 'validated' OR k.convalidated)) THEN 'landed' ELSE 'not_landed' END
  WHEN u.kind = 'constraint_absent' THEN CASE WHEN EXISTS (
       SELECT 1 FROM pg_catalog.pg_constraint k
        WHERE k.conrelid = r.oid AND k.conname = u.con) THEN 'not_landed' ELSE 'landed' END
  WHEN u.kind = 'column_absent' THEN CASE WHEN a.attnum IS NULL THEN 'landed'
       ELSE 'not_landed' END
  WHEN u.kind = 'column_renamed' THEN CASE WHEN a.attnum IS NULL AND EXISTS (
       SELECT 1 FROM pg_catalog.pg_attribute n
        WHERE n.attrelid = r.oid AND n.attname = u.newname AND n.attnum > 0
          AND NOT n.attisdropped) THEN 'landed' ELSE 'not_landed' END
  WHEN a.attnum IS NULL THEN 'not_landed'
  WHEN u.kind = 'column_present' THEN CASE WHEN u.typ = ''
       OR pg_catalog.format_type(a.atttypid, a.atttypmod) = u.typ THEN 'landed'
       ELSE 'not_landed' END
  WHEN u.kind = 'column_type' THEN CASE
       WHEN pg_catalog.format_type(a.atttypid, a.atttypmod) = u.typ THEN 'landed'
       ELSE 'not_landed' END
  WHEN u.kind = 'not_null' THEN CASE WHEN a.attnotnull THEN 'landed' ELSE 'not_landed' END
  WHEN u.kind = 'nullable' THEN CASE WHEN a.attnotnull THEN 'not_landed' ELSE 'landed' END
  ELSE 'unknown' END
FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[],
            $7::text[], $8::text[])
     WITH ORDINALITY AS u(kind, schema, rel, col, newname, typ, idx, con, ord)
LEFT JOIN pg_catalog.pg_namespace ns ON ns.nspname = u.schema
LEFT JOIN pg_catalog.pg_class r ON r.relnamespace = ns.oid AND r.relname = u.rel
     AND r.relkind IN ('r', 'p', 'v', 'm', 'f')
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = r.oid AND a.attname = u.col
     AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY u.ord`

var postKinds = map[string]bool{changereview.PostIndexValid: true,
	changereview.PostIndexAbsent: true, changereview.PostColumnPresent: true,
	changereview.PostColumnAbsent: true, changereview.PostColumnRenamed: true,
	changereview.PostColumnType: true, changereview.PostNotNull: true,
	changereview.PostNullable: true, changereview.PostConstraint: true,
	changereview.PostConstraintAbsent: true, changereview.PostRelationPresent: true,
	changereview.PostRelationAbsent: true, changereview.PostRelationRenamed: true}

// EvaluatePostConditions checks every post-condition against the catalog
// in one bounded statement (q is a catalogread.Reader in production). An
// unknown kind or a relation without a schema is ErrInvalid.
func EvaluatePostConditions(ctx context.Context, q catalogread.Querier,
	pcs []changereview.PostCondition) ([]PostResult, error) {
	if len(pcs) == 0 {
		return []PostResult{}, nil
	}
	cols := make([][]string, 8)
	for _, pc := range pcs {
		schema, rel, ok := splitQualified(pc.Relation)
		if !postKinds[pc.Kind] {
			return nil, fmt.Errorf("%w: post-condition kind %q", changereview.ErrInvalid,
				pc.Kind)
		}
		if !ok {
			return nil, fmt.Errorf("%w: post-condition relation %q is not schema.name",
				changereview.ErrInvalid, pc.Relation)
		}
		for i, v := range []string{pc.Kind, schema, rel, pc.Column, pc.NewName, pc.Type,
			pc.Index, pc.Constraint} {
			cols[i] = append(cols[i], v)
		}
	}
	rows, err := q.Query(ctx, postConditionsSQL, cols[0], cols[1], cols[2], cols[3],
		cols[4], cols[5], cols[6], cols[7])
	if err != nil {
		return nil, fmt.Errorf("evaluate post-conditions: %w", err)
	}
	defer rows.Close()
	out := make([]PostResult, 0, len(pcs))
	for rows.Next() {
		var ord int
		var state string
		if err := rows.Scan(&ord, &state); err != nil {
			return nil, fmt.Errorf("evaluate post-conditions: %w", err)
		}
		out = append(out, PostResult{Condition: pcs[ord-1], State: state})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("evaluate post-conditions: %w", err)
	}
	return out, nil
}

// Landing is the state of a review's post-conditions together: landed
// when all are, invalid when any is, not landed otherwise ("" for none).
func Landing(results []PostResult) string {
	if len(results) == 0 {
		return ""
	}
	state := PostLanded
	for _, r := range results {
		switch r.State {
		case PostInvalid:
			return PostInvalid
		case PostLanded:
		default:
			state = PostNotLanded
		}
	}
	return state
}

// AlreadyLanded reports whether every post-condition of exp already holds
// (a migration with nothing left to do: landing not_applicable). Without
// post-conditions it is false.
func AlreadyLanded(ctx context.Context, q catalogread.Querier,
	exp changereview.Expectations) (bool, error) {
	if len(exp.PostConditions) == 0 {
		return false, nil
	}
	res, err := EvaluatePostConditions(ctx, q, exp.PostConditions)
	if err != nil {
		return false, err
	}
	return Landing(res) == PostLanded, nil
}

// atReview is what exp's post-conditions read now, at review time
// (changereview.AtReviewLanded or AtReviewUnmet), or "" without one or
// when the read fails (logged: it bears on the receipt, not the risk).
func (r *run) atReview(exp changereview.Expectations) string {
	if len(exp.PostConditions) == 0 || r.ctx.Err() != nil {
		return ""
	}
	landed, err := AlreadyLanded(r.ctx, r.reader, exp)
	switch {
	case err != nil:
		r.e.logf("change review impact: post-conditions at review time: %v", err)
		return ""
	case landed:
		return changereview.AtReviewLanded
	}
	return changereview.AtReviewUnmet
}
