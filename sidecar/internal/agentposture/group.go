package agentposture

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// groupThreshold is the most findings AP-03, AP-05 and AP-06 report on
// their own in one schema; past it, the schema gets one finding instead,
// so a large schema does not flood the findings list.
const groupThreshold = 20

// groupShown is how many objects a schema finding's evidence lists.
const groupShown = 50

// groupBySchema makes one finding per item, except that a schema with
// more than groupThreshold items gets one finding from many, in the place
// of its first item. Items keep their order.
func groupBySchema[T any](items []T, schemaOf func(T) string, one func(T) Finding,
	many func(schema string, items []T) Finding) []Finding {
	by := map[string][]T{}
	for _, it := range items {
		s := schemaOf(it)
		by[s] = append(by[s], it)
	}
	out := make([]Finding, 0, len(items))
	done := map[string]bool{}
	for _, it := range items {
		s := schemaOf(it)
		switch {
		case len(by[s]) <= groupThreshold:
			out = append(out, one(it))
		case !done[s]:
			done[s] = true
			out = append(out, many(s, by[s]))
		}
	}
	return out
}

// groupEvidence is a schema finding's evidence: the count of objects
// (what they are), then the first groupShown of them.
func groupEvidence(source, schema, what string, objs []Evidence) []Evidence {
	count := fmt.Sprintf("%d %s", len(objs), what)
	if len(objs) > groupShown {
		count += fmt.Sprintf("; the first %d follow", groupShown)
	}
	out := []Evidence{{Source: source, Ref: schema, Detail: count}}
	return append(out, objs[:min(len(objs), groupShown)]...)
}

// dollarQuoted dollar-quotes body for a DO block, with a tag the body
// does not contain.
func dollarQuoted(body string) string {
	tag := "$fix$"
	for i := 1; strings.Contains(body, tag); i++ {
		tag = "$fix" + strconv.Itoa(i) + "$"
	}
	return tag + "\n" + body + "\n" + tag
}

// sqlLiteral is s as a standard SQL string literal.
func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// schemaLiteral is schema as a regnamespace literal for a fix script.
func schemaLiteral(schema string) string {
	return sqlLiteral(sqlident.Ident(schema)) + "::pg_catalog.regnamespace"
}
