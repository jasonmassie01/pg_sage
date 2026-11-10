package facts

import (
	"regexp"
	"sort"
	"strings"
)

// testSchemaRegex names test schemas (PostgreSQL regex): a test-ish
// prefix or suffix, or a "_test_" infix.
const testSchemaRegex = `^(test|tests|testing|tmp|temp|pytest|ci)[_-]|` +
	`[_-](test|tests|tmp|temp)$|[_-]test[_-]`

// testSchemaName is testSchemaRegex compiled for Go, matched case
// insensitively like the catalog query's ~*.
var testSchemaName = regexp.MustCompile(`(?i)` + testSchemaRegex)

// TestSchemaPattern is the PostgreSQL regular expression (case
// insensitive) that names test schemas.
func TestSchemaPattern() string { return testSchemaRegex }

// IsTestSchemaName reports whether name looks like a test schema (the
// fixture detector's rule), so callers can treat it as test traffic
// before any fact is confirmed.
func IsTestSchemaName(name string) bool { return testSchemaName.MatchString(name) }

// familyKey is a schema's name up to its generated suffix (a run after
// the last separator that holds a digit and is at least 4 long), or ""
// when it has none.
func familyKey(name string) string {
	i := strings.LastIndexAny(name, "_-")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	suffix := name[i+1:]
	if len(suffix) < 4 || !strings.ContainsAny(suffix, "0123456789") {
		return ""
	}
	return name[:i+1]
}

// fixtureRule is how one caller sees its test schemas: a schema's name,
// whether it has been quiet long enough, and how to propose a subject.
type fixtureRule[T any] struct {
	name    func(T) string
	quiet   func(T) bool
	propose func(subject string, members []T) Proposal
}

// fixtureProposals groups the test-named schemas by family key, in key
// order, and proposes each group by the shared family rule.
func fixtureProposals[T any](all []T, r fixtureRule[T]) []Proposal {
	groups := map[string][]T{}
	var keys []string
	var named []T
	for _, s := range all {
		if !IsTestSchemaName(r.name(s)) {
			continue
		}
		named = append(named, s)
		key := familyKey(r.name(s))
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], s)
	}
	sort.Strings(keys)
	var out []Proposal
	for _, key := range keys {
		out = append(out, familyProposals(key, groups[key], named, r)...)
	}
	return out
}

// familyProposals are one family pattern for two or more schemas under a
// key, once every test-named schema the pattern would match is quiet (a
// live one, member or not, holds the family: no flood of single
// proposals, and no pattern hiding live traffic); else each quiet schema
// by name.
func familyProposals[T any](key string, members, all []T, r fixtureRule[T]) []Proposal {
	if key != "" && len(members) >= 2 {
		for _, s := range all {
			if strings.HasPrefix(r.name(s), key) && !r.quiet(s) {
				return nil
			}
		}
		return []Proposal{r.propose(quoteIdent(key+"*"), members)}
	}
	var out []Proposal
	for _, m := range members {
		if r.quiet(m) {
			out = append(out, r.propose(quoteIdent(r.name(m)), []T{m}))
		}
	}
	return out
}
