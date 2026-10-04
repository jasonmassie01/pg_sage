package optimizer

import "testing"

// Subsumes is CoveredBy with predicate implication: a wider index whose
// predicate is a subset of the narrower one's conjuncts serves every
// lookup the narrower one would (lifeos 1.10.0, actions 6410 and 6411).
func TestSubsumes(t *testing.T) {
	live := "CREATE INDEX idx_memories_live_status_type_quality ON public.memories " +
		"USING btree (status, fact_type, quality_score) WHERE ((valid_to IS NULL) AND " +
		"(deleted_at IS NULL) AND (quality_score IS NOT NULL))"
	current := "CREATE INDEX CONCURRENTLY idx_memories_status_type_quality_current ON " +
		"public.memories (status, fact_type, quality_score) WHERE valid_to IS NULL AND " +
		"deleted_at IS NULL"
	for _, tc := range []struct {
		name            string
		wider, narrower string
		want            bool
	}{
		{"weaker predicate, same keys", current, live, true},
		{"stronger predicate does not subsume", live, current, false},
		{"equal", current, current, true},
		{"no predicate subsumes a partial index on its prefix",
			"CREATE INDEX a ON public.m USING btree (status, fact_type)",
			"CREATE INDEX b ON public.m (status) WHERE deleted_at IS NULL", true},
		{"a partial index never subsumes a full one",
			"CREATE INDEX a ON public.m (status) WHERE deleted_at IS NULL",
			"CREATE INDEX b ON public.m (status)", false},
		{"OR predicates compare exactly",
			"CREATE INDEX a ON public.m (status) WHERE a IS NULL OR b IS NULL",
			"CREATE INDEX b ON public.m (status) WHERE a IS NULL OR b IS NULL AND c = 1",
			false},
		{"different leading key", "CREATE INDEX a ON public.m (fact_type, status)",
			"CREATE INDEX b ON public.m (status)", false},
		{"a unique index is a constraint, not redundant",
			"CREATE INDEX a ON public.m (status, fact_type)",
			"CREATE UNIQUE INDEX b ON public.m (status)", false},
		{"unparseable", "not an index", current, false},
	} {
		if got := Subsumes(tc.wider, tc.narrower); got != tc.want {
			t.Errorf("%s: Subsumes = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLeadingKey(t *testing.T) {
	if k, ok := LeadingKey("CREATE INDEX a ON public.m USING btree (Status, x)"); !ok ||
		k != "status" {
		t.Fatalf("leading key = %q %v", k, ok)
	}
	if _, ok := LeadingKey("DROP INDEX a"); ok {
		t.Fatal("not an index")
	}
}
