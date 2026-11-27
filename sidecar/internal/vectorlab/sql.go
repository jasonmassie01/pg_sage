package vectorlab

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// buildSQL returns the k-nearest query: an HNSW candidate, or for exact the
// index-proof baseline with k+1 rows so a tie at the k-th distance shows.
func buildSQL(
	m Manifest, extensionSchema, vectorType string, q Query, exact bool,
) (string, []any) {
	s := newSearch(m, extensionSchema, vectorType, q)
	limit, order := m.K, s.distance
	if exact {
		limit, order = m.K+1, exactOrder(s.distance)
	}
	s.args[1] = limit
	return s.query(m, order), s.args
}

// buildBoundarySQL returns every exact row at or nearer than the k-th
// distance, bounded at 2k rows, for tie-aware recall.
func buildBoundarySQL(
	m Manifest, extensionSchema, vectorType string, q Query, distance float64,
) (string, []any) {
	s := newSearch(m, extensionSchema, vectorType, q)
	s.args[1] = 2 * m.K
	s.args = append(s.args, distance)
	s.predicates = append(s.predicates, s.distance+fmt.Sprintf(" <= $%d", len(s.args)))
	return s.query(m, exactOrder(s.distance)), s.args
}

type search struct {
	distance   string
	predicates []string
	args       []any
}

func newSearch(m Manifest, extensionSchema, vectorType string, q Query) search {
	operator := map[string]string{"l2": "<->", "cosine": "<=>", "inner_product": "<#>"}[m.Distance]
	embedding := pgx.Identifier{m.VectorColumn}.Sanitize()
	extension := pgx.Identifier{extensionSchema}.Sanitize()
	s := search{
		distance: embedding + " OPERATOR(" + extension + "." + operator + ") $1::" +
			pgx.Identifier{extensionSchema, vectorType}.Sanitize(),
		predicates: []string{embedding + " IS NOT NULL"},
		args:       []any{vectorText(q.Vector), nil},
	}
	for i, column := range m.FilterColumns {
		s.predicates = append(s.predicates, pgx.Identifier{column}.Sanitize()+
			fmt.Sprintf(" = $%d", i+3))
		s.args = append(s.args, q.Filters[i])
	}
	return s
}

func (s search) query(m Manifest, order string) string {
	return "SELECT " + pgx.Identifier{m.IDColumn}.Sanitize() + "::text, " + s.distance +
		" FROM " + pgx.Identifier{m.Schema, m.Table}.Sanitize() +
		" WHERE " + strings.Join(s.predicates, " AND ") + " ORDER BY " + order + " LIMIT $2"
}

// exactOrder hides the distance operator from the planner so no index can
// serve the ORDER BY.
func exactOrder(distance string) string { return "(" + distance + ") + 0" }

func vectorText(vector []float64) string {
	parts := make([]string, len(vector))
	for i, v := range vector {
		parts[i] = strconv.FormatFloat(v, 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
