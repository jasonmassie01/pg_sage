package optimizer

import "testing"

// Dogfood round 2 item 5: CoveredBy is the one coverage rule (the executor
// applies it before queueing or running any index create). A UNIQUE
// candidate is a constraint, not only a lookup path: only a unique index
// with exactly its keys and predicate covers it.
func TestCoveredByUniqueCandidates(t *testing.T) {
	cases := []struct {
		name, ddl, existing string
		covered             bool
	}{
		{"unique candidate beside a plain index",
			"CREATE UNIQUE INDEX CONCURRENTLY c ON public.t (a)",
			"CREATE INDEX e ON public.t USING btree (a)", false},
		{"unique candidate beside a unique index on more keys",
			"CREATE UNIQUE INDEX CONCURRENTLY c ON public.t (a)",
			"CREATE UNIQUE INDEX e ON public.t USING btree (a, b)", false},
		{"unique candidate beside the same unique index",
			"CREATE UNIQUE INDEX CONCURRENTLY c ON public.t (a)",
			"CREATE UNIQUE INDEX e ON public.t USING btree (a)", true},
		{"unique candidate beside a unique index with INCLUDE",
			"CREATE UNIQUE INDEX CONCURRENTLY c ON public.t (a)",
			"CREATE UNIQUE INDEX e ON public.t USING btree (a) INCLUDE (b)", true},
		{"plain candidate beside a unique index (lookups served)",
			"CREATE INDEX CONCURRENTLY c ON public.t (a)",
			"CREATE UNIQUE INDEX e ON public.t USING btree (a, b)", true},
		{"lifeos queue 6", "CREATE INDEX CONCURRENTLY graph_nodes_node_type_name_idx " +
			"ON public.graph_nodes (node_type, name);",
			"CREATE INDEX idx_graph_nodes_node_type_name_covering ON public.graph_nodes " +
				"USING btree (node_type, name) INCLUDE (id)", true},
		{"garbage candidate", "VACUUM t", "CREATE INDEX e ON public.t USING btree (a)", false},
		{"garbage existing", "CREATE INDEX CONCURRENTLY c ON public.t (a)", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoveredBy(tc.ddl, tc.existing); got != tc.covered {
				t.Fatalf("CoveredBy = %v, want %v", got, tc.covered)
			}
		})
	}
}
