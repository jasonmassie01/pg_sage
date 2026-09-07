package vectorlab

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func buildSQL(m Manifest, extensionSchema string, q Query, exact bool) (string, []any) {
	operator := map[string]string{"l2": "<->", "cosine": "<=>", "inner_product": "<#>"}[m.Distance]
	embedding := pgx.Identifier{m.VectorColumn}.Sanitize()
	extension := pgx.Identifier{extensionSchema}.Sanitize()
	distance := embedding + " OPERATOR(" + extension + "." + operator + ") $1::" +
		extension + ".vector"
	limit := m.K
	order := distance
	if exact {
		limit++
		order = "(" + distance + ") + 0"
	}
	args := []any{vectorText(q.Vector), limit}
	predicates := []string{embedding + " IS NOT NULL"}
	for i, column := range m.FilterColumns {
		predicates = append(predicates, pgx.Identifier{column}.Sanitize()+
			fmt.Sprintf(" = $%d", i+3))
		args = append(args, q.Filters[i])
	}
	query := "SELECT " + pgx.Identifier{m.IDColumn}.Sanitize() + "::text, " + distance +
		" FROM " + pgx.Identifier{m.Schema, m.Table}.Sanitize() +
		" WHERE " + strings.Join(predicates, " AND ") + " ORDER BY " + order + " LIMIT $2"
	return query, args
}

func vectorText(vector []float64) string {
	parts := make([]string, len(vector))
	for i, v := range vector {
		parts[i] = strconv.FormatFloat(v, 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
