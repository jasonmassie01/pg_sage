package optimizer

import (
	"strings"
	"testing"
)

// Dogfood lifeos 1.8.3 (action 6400): a legacy finding proposed
// graph_nodes (node_type, name) after action 6383 had built
// (node_type, name) INCLUDE (id). The duplicate gate compared whole shapes
// only, so a candidate an existing index already serves was not rejected,
// and the duplicate-index rule would later drop the redundant build.

var coveredCases = []struct {
	name, ddl, existing string
	covered             bool
}{
	{"lifeos 6400: keys equal, existing includes more",
		"CREATE INDEX CONCURRENTLY c ON public.orders (node_type, name)",
		"CREATE INDEX existing ON public.orders USING btree (node_type, name) INCLUDE (id)",
		true},
	{"btree key prefix", "CREATE INDEX CONCURRENTLY c ON public.orders (status)",
		"CREATE INDEX existing ON public.orders USING btree (status, created_at)", true},
	{"include served by existing keys",
		"CREATE INDEX CONCURRENTLY c ON public.orders (status) INCLUDE (created_at)",
		"CREATE INDEX existing ON public.orders USING btree (status, created_at)", true},
	{"include subset", "CREATE INDEX CONCURRENTLY c ON public.orders (a) INCLUDE (x)",
		"CREATE INDEX existing ON public.orders USING btree (a) INCLUDE (y, x)", true},
	{"unique existing serves lookups", "CREATE INDEX CONCURRENTLY c ON public.orders (id)",
		"CREATE UNIQUE INDEX existing ON public.orders USING btree (id)", true},
	{"same predicate, longer key",
		"CREATE INDEX CONCURRENTLY c ON public.orders (status) WHERE status = 'open'",
		"CREATE INDEX existing ON public.orders USING btree (status, id) " +
			"WHERE (status = 'open'::text)", true},
	{"candidate longer than existing",
		"CREATE INDEX CONCURRENTLY c ON public.orders (status, created_at)",
		"CREATE INDEX existing ON public.orders USING btree (status)", false},
	{"not a prefix", "CREATE INDEX CONCURRENTLY c ON public.orders (created_at)",
		"CREATE INDEX existing ON public.orders USING btree (status, created_at)", false},
	{"include not served",
		"CREATE INDEX CONCURRENTLY c ON public.orders (status) INCLUDE (total)",
		"CREATE INDEX existing ON public.orders USING btree (status, created_at)", false},
	{"partial candidate, full existing",
		"CREATE INDEX CONCURRENTLY c ON public.orders (status) WHERE deleted_at IS NULL",
		"CREATE INDEX existing ON public.orders USING btree (status, id)", false},
	{"full candidate, partial existing",
		"CREATE INDEX CONCURRENTLY c ON public.orders (status)",
		"CREATE INDEX existing ON public.orders USING btree (status, id) " +
			"WHERE (deleted_at IS NULL)", false},
	{"gin prefix is not coverage",
		"CREATE INDEX CONCURRENTLY c ON public.orders USING gin (tags)",
		"CREATE INDEX existing ON public.orders USING gin (tags, labels)", false},
	{"different method", "CREATE INDEX CONCURRENTLY c ON public.orders USING hash (status)",
		"CREATE INDEX existing ON public.orders USING btree (status, id)", false},
	{"expression prefix", "CREATE INDEX CONCURRENTLY c ON public.orders ((lower(email)))",
		"CREATE INDEX existing ON public.orders USING btree (lower((email)::text), id)", true},
}

func TestCheckDuplicate_RejectsCandidateAnExistingIndexCovers(t *testing.T) {
	v := newTestValidator(nil)
	for _, c := range coveredCases {
		ok, reason := v.checkDuplicate(Recommendation{DDL: c.ddl},
			dupContext(validIndex(c.existing)))
		if ok == c.covered {
			t.Errorf("%s: ok=%t (%s), want covered=%t", c.name, ok, reason, c.covered)
		}
		if c.covered && !strings.Contains(reason, "existing") {
			t.Errorf("%s: reason %q does not name the covering index", c.name, reason)
		}
	}
}

// An invalid (failed CONCURRENTLY build) index covers nothing.
func TestCheckDuplicate_InvalidIndexCoversNothing(t *testing.T) {
	v := newTestValidator(nil)
	existing := validIndex("CREATE INDEX existing ON public.orders USING btree " +
		"(node_type, name) INCLUDE (id)")
	existing.IsValid = false
	ok, reason := v.checkDuplicate(Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY c ON public.orders (node_type, name)"},
		dupContext(existing))
	if !ok {
		t.Fatalf("an invalid index rejected the candidate: %s", reason)
	}
}
