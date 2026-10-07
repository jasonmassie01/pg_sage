package fleetlearn

// sqlKeywords are kept verbatim in a normalized statement; every other
// word is an identifier and becomes "x". Type names are deliberately not
// keywords: a cast's type is schema vocabulary like any identifier.
var sqlKeywords = setOf(
	"select", "from", "where", "and", "or", "not", "in", "is", "null", "as",
	"join", "inner", "left", "right", "full", "outer", "cross", "lateral", "on",
	"using", "group", "by", "order", "having", "limit", "offset", "fetch", "first",
	"next", "rows", "row", "only", "asc", "desc", "nulls", "last", "distinct", "all",
	"any", "some", "exists", "between", "like", "ilike", "similar", "to", "escape",
	"case", "when", "then", "else", "end", "union", "intersect", "except", "with",
	"recursive", "insert", "into", "values", "default", "update", "set", "delete",
	"returning", "conflict", "do", "nothing", "for", "share", "no", "key", "nowait",
	"skip", "locked", "window", "over", "partition", "filter", "within", "true",
	"false", "cast", "interval", "array", "coalesce", "count", "sum", "avg", "min",
	"max", "now", "current_timestamp", "current_date", "of", "at", "time", "zone",
	"materialized", "merge", "matched", "tablesample", "collate",
)

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}
