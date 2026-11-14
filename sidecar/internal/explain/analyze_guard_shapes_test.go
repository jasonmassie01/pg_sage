package explain

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The analyze guard has two halves: the parser turns the query into a
// sqlast.ReadQuery (cgo only), and the catalog judges that shape (plain
// Go and SQL). The live guard tests run the catalog half on every build by
// feeding it the shape libpg_query gives each query; the cgo twins run the
// same checks with the real parser, which pins these shapes.

// inspectFn is a parse-shape source; nil means the real parser.
type inspectFn = func(string) (sqlast.ReadQuery, error)

func qn(schema, name string) sqlast.QualifiedName {
	return sqlast.QualifiedName{Schema: schema, Name: name}
}

func names(list ...sqlast.QualifiedName) []sqlast.QualifiedName { return list }

// concurrentQueries is how many goroutines the concurrency test runs.
const concurrentQueries = 16

func concurrentQuery(i int) string {
	if i%2 == 0 {
		return fmt.Sprintf("SELECT v AS c%d FROM public.eg_probe", i)
	}
	return fmt.Sprintf("SELECT eg_volatile() AS c%d", i)
}

// recordedShapes maps each query text the live tests send (and each
// default expression the guard renders) to its parse shape.
func recordedShapes() map[string]sqlast.ReadQuery {
	probe := names(qn("public", "eg_probe"))
	shapes := map[string]sqlast.ReadQuery{
		"SELECT v FROM public.eg_probe":   {Relations: probe},
		"SELECT nextval('public.eg_seq')": {Functions: names(qn("", "nextval"))},
		"SELECT pg_advisory_lock(4242)":   {Functions: names(qn("", "pg_advisory_lock"))},
		"SELECT eg_volatile()":            {Functions: names(qn("", "eg_volatile"))},
		"SELECT 1 === 2":                  {Operators: names(qn("", "==="))},
		"SELECT eg_defaulted()":           {Functions: names(qn("", "eg_defaulted"))},
		"SELECT eg_const_default()":       {Functions: names(qn("", "eg_const_default"))},
		"SELECT 7":                        {},
		"SELECT eg_flip(1)":               {Functions: names(qn("", "eg_flip"))},
		"SELECT * FROM eg_chain1":         {Relations: names(qn("", "eg_chain1"))},
		"SELECT * FROM eg_chain2":         {Relations: names(qn("", "eg_chain2"))},
		"SELECT eg_missing_fn()":          {Functions: names(qn("", "eg_missing_fn"))},
		"SELECT * FROM eg_missing_rel":    {Relations: names(qn("", "eg_missing_rel"))},
		"SELECT 1 OPERATOR(public.##?) 2": {Operators: names(qn("public", "##?"))},
		"SELECT 1::eg_missing_type":       {Types: names(qn("", "eg_missing_type"))},
		"SELECT length(x) FROM public.eg_cast_t": {Functions: names(qn("", "length")),
			Relations: names(qn("public", "eg_cast_t"))},
		"SELECT * FROM eg_probe FOR UPDATE": {Relations: names(qn("", "eg_probe")),
			LockingClause: true},
		"WITH d AS (DELETE FROM eg_probe RETURNING v) SELECT * FROM d": {
			Relations: names(qn("", "d"), qn("", "eg_probe")), CTENames: []string{"d"},
			ModifiesData: true},
		"SELECT pg_try_advisory_lock((4243)::bigint)": {
			Functions: names(qn("", "pg_try_advisory_lock")),
			Types:     names(qn("pg_catalog", "int8"))},
		"SELECT count(*) FROM generate_series(1, 10) g": {
			Functions: names(qn("", "count"), qn("", "generate_series"))},
		"SELECT count(*) FROM generate_series(1, 2000000000) g": {
			Functions: names(qn("", "count"), qn("", "generate_series"))},
	}
	for query, shape := range domainShapes {
		shapes[query] = shape
	}
	for i := 0; i < concurrentQueries; i++ {
		shape := sqlast.ReadQuery{Relations: probe}
		if i%2 == 1 {
			shape = sqlast.ReadQuery{Functions: names(qn("", "eg_volatile"))}
		}
		shapes[concurrentQuery(i)] = shape
	}
	return shapes
}

// viewShapes are matched by substring: pg_get_viewdef's layout differs
// between majors, but each chain view names exactly one relation.
var viewShapes = []struct {
	match string
	shape sqlast.ReadQuery
}{
	{"eg_chain2", sqlast.ReadQuery{Relations: names(qn("", "eg_chain2"))}},
	{"eg_chain3", sqlast.ReadQuery{Relations: names(qn("", "eg_chain3"))}},
	{"eg_chain4", sqlast.ReadQuery{Relations: names(qn("", "eg_chain4"))}},
	{"eg_chain5", sqlast.ReadQuery{Relations: names(qn("", "eg_chain5"))}},
	{"eg_probe", sqlast.ReadQuery{Relations: names(qn("", "eg_probe"))}},
}

// recordedInspector serves recordedShapes; any other text is an error,
// so the guard refuses it (the same fail-closed path as a parse error).
// The map is read-only after construction, so it is safe for concurrent
// calls.
func recordedInspector() inspectFn {
	shapes := recordedShapes()
	return func(sql string) (sqlast.ReadQuery, error) {
		if shape, ok := shapes[strings.TrimSpace(sql)]; ok {
			return shape, nil
		}
		if strings.Contains(sql, "FROM") && !strings.Contains(sql, "(") {
			for _, v := range viewShapes {
				if strings.Contains(sql, v.match) {
					return v.shape, nil
				}
			}
		}
		return sqlast.ReadQuery{}, fmt.Errorf("no recorded parse shape for %q", sql)
	}
}
