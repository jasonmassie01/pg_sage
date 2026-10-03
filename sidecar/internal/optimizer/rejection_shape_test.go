package optimizer

import (
	"strings"
	"testing"
)

// Rejection memory identifies an index idea by its normalized shape: access
// method, ordered keys with opclass/collation/order, predicate and the
// INCLUDE columns as a set. The index name and the table qualification are
// not part of it. A candidate whose INCLUDE set is a subset or superset of a
// rejected one with the same keys, method and predicate is the same idea
// (lifeos 2026-10-03: 18 proposals of one ai_claims index in three hours).

// No concurrent access tests here: shape parsing is pure and shares nothing.

const lifeosBase = "CREATE INDEX CONCURRENTLY %s ON public.ai_claims USING btree " +
	"(evidence_event_ids_json text_pattern_ops) INCLUDE (%s)"

func lifeosDDL(name, include string) string {
	return strings.Replace(strings.Replace(lifeosBase, "%s", name, 1), "%s", include, 1)
}

func mustShape(t *testing.T, ddl string) candidateShape {
	t.Helper()
	s, err := shapeOfCandidate(ddl)
	if err != nil {
		t.Fatalf("shapeOfCandidate(%q): %v", ddl, err)
	}
	return s
}

func TestCandidateShape_LifeosVariantsAreOneIdea(t *testing.T) {
	first := mustShape(t, lifeosDDL("ai_claims_evidence_pattern_idx", "id, status"))
	for _, ddl := range []string{
		lifeosDDL("ai_claims_evidence_event_ids_pattern_idx", "id"),
		lifeosDDL("ai_claims_evidence_prefix_idx", "status, id"),
	} {
		if s := mustShape(t, ddl); !first.sameIdea(s) || !s.sameIdea(first) {
			t.Errorf("lifeos variant is a new idea:\n %+v\n %+v", first, s)
		}
	}
	want := "btree (evidence_event_ids_json text_pattern_ops) INCLUDE (id, status)"
	if got := first.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestCandidateShape_SameIdea(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"name differs", "CREATE INDEX CONCURRENTLY a ON t (x)",
			"CREATE INDEX CONCURRENTLY b ON t (x)"},
		{"include order", "CREATE INDEX i ON t (x) INCLUDE (a, b)",
			"CREATE INDEX i ON t (x) INCLUDE (b, a)"},
		{"include subset", "CREATE INDEX i ON t (x) INCLUDE (a)",
			"CREATE INDEX i ON t (x) INCLUDE (a, b)"},
		{"include superset", "CREATE INDEX i ON t (x) INCLUDE (a, b, c)",
			"CREATE INDEX i ON t (x) INCLUDE (b)"},
		{"no include vs include", "CREATE INDEX i ON t (x)",
			"CREATE INDEX i ON t (x) INCLUDE (a)"},
		{"duplicate include column", "CREATE INDEX i ON t (x) INCLUDE (a, a)",
			"CREATE INDEX i ON t (x) INCLUDE (a)"},
		{"schema qualified table", "CREATE INDEX i ON ai_claims (x)",
			"CREATE INDEX i ON public.ai_claims (x)"},
		{"quoted qualified table", `CREATE INDEX i ON "public"."ai_claims" (x)`,
			"CREATE INDEX i ON public.ai_claims (x)"},
		{"method omitted", "CREATE INDEX i ON t (x)", "CREATE INDEX i ON t USING BTREE (x)"},
		{"case and spacing", "create index i on t ( Evidence_Json   TEXT_PATTERN_OPS )",
			"CREATE INDEX i ON t (evidence_json text_pattern_ops)"},
		{"quoted lowercase column", `CREATE INDEX i ON t ("status") INCLUDE ("id")`,
			"CREATE INDEX i ON t (status) INCLUDE (id)"},
		{"explicit default order", "CREATE INDEX i ON t (x ASC NULLS LAST)",
			"CREATE INDEX i ON t (x)"},
		{"desc default nulls", "CREATE INDEX i ON t (x DESC NULLS FIRST)",
			"CREATE INDEX i ON t (x DESC)"},
		{"redundant expression parens", "CREATE INDEX i ON t (((lower(email))))",
			"CREATE INDEX i ON t (lower( email ))"},
		{"pg_catalog opclass", "CREATE INDEX i ON t (x pg_catalog.text_pattern_ops)",
			"CREATE INDEX i ON t (x text_pattern_ops)"},
		{"collation spelling", `CREATE INDEX i ON t (x COLLATE pg_catalog."C")`,
			`CREATE INDEX i ON t (x collate "C")`},
		{"concurrently, if not exists, semicolon",
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON t (x);",
			"CREATE INDEX i ON t (x)"},
		{"predicate parens and spacing", "CREATE INDEX i ON t (x) WHERE (status='open')",
			"CREATE INDEX i ON t (x) WHERE status = 'open'"},
		{"predicate conjunct order", "CREATE INDEX i ON t (x) WHERE a = 1 AND b = 2",
			"CREATE INDEX i ON t (x) WHERE (b = 2) and (a = 1)"},
		{"not-equal spelling", "CREATE INDEX i ON t (x) WHERE s != 'x'",
			"CREATE INDEX i ON t (x) WHERE s <> 'x'"},
		{"between kept whole", "CREATE INDEX i ON t (x) WHERE a BETWEEN 1 AND 5",
			"CREATE INDEX i ON t (x) WHERE a between 1 and 5"},
	}
	for _, c := range cases {
		a, b := mustShape(t, c.a), mustShape(t, c.b)
		if !a.sameIdea(b) || !b.sameIdea(a) {
			t.Errorf("%s: want the same idea\n %q -> %+v\n %q -> %+v", c.name, c.a, a, c.b, b)
		}
	}
}

func TestCandidateShape_DifferentIdeas(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"different opclass", "CREATE INDEX i ON t (x text_pattern_ops)",
			"CREATE INDEX i ON t (x varchar_pattern_ops)"},
		{"opclass vs default", "CREATE INDEX i ON t (x text_pattern_ops)",
			"CREATE INDEX i ON t (x)"},
		{"opclass params", "CREATE INDEX i ON t USING gist (x gist_trgm_ops(siglen=32))",
			"CREATE INDEX i ON t USING gist (x gist_trgm_ops(siglen=64))"},
		{"key order", "CREATE INDEX i ON t (a, b)", "CREATE INDEX i ON t (b, a)"},
		{"key vs include", "CREATE INDEX i ON t (a) INCLUDE (b)", "CREATE INDEX i ON t (a, b)"},
		{"extra key", "CREATE INDEX i ON t (a)", "CREATE INDEX i ON t (a, b)"},
		{"method", "CREATE INDEX i ON t (x)", "CREATE INDEX i ON t USING hash (x)"},
		{"gin vs gist", "CREATE INDEX i ON t USING gin (x)", "CREATE INDEX i ON t USING gist (x)"},
		{"predicate value", "CREATE INDEX i ON t (x) WHERE status = 'open'",
			"CREATE INDEX i ON t (x) WHERE status = 'closed'"},
		{"predicate vs none", "CREATE INDEX i ON t (x) WHERE status = 'open'",
			"CREATE INDEX i ON t (x)"},
		{"literal case", "CREATE INDEX i ON t (x) WHERE status = 'Open'",
			"CREATE INDEX i ON t (x) WHERE status = 'open'"},
		{"or/and grouping", "CREATE INDEX i ON t (x) WHERE a = 1 OR b = 2 AND c = 3",
			"CREATE INDEX i ON t (x) WHERE (a = 1 OR b = 2) AND c = 3"},
		{"quoted mixed case column", `CREATE INDEX i ON t ("Status")`,
			"CREATE INDEX i ON t (status)"},
		{"descending", "CREATE INDEX i ON t (x DESC)", "CREATE INDEX i ON t (x)"},
		{"non-default nulls", "CREATE INDEX i ON t (x NULLS FIRST)", "CREATE INDEX i ON t (x)"},
		{"collation", `CREATE INDEX i ON t (x COLLATE "C")`, "CREATE INDEX i ON t (x)"},
		{"overlapping include sets", "CREATE INDEX i ON t (x) INCLUDE (a, b)",
			"CREATE INDEX i ON t (x) INCLUDE (a, c)"},
		{"expression", "CREATE INDEX i ON t (lower(email))",
			"CREATE INDEX i ON t (upper(email))"},
		{"expression vs column", "CREATE INDEX i ON t (lower(email))",
			"CREATE INDEX i ON t (email)"},
		{"cast", "CREATE INDEX i ON t (((payload->>'k')::int))",
			"CREATE INDEX i ON t ((payload->>'k'))"},
	}
	for _, c := range cases {
		a, b := mustShape(t, c.a), mustShape(t, c.b)
		if a.sameIdea(b) || b.sameIdea(a) {
			t.Errorf("%s: want different ideas\n %q -> %+v\n %q -> %+v", c.name, c.a, a, c.b, b)
		}
	}
}

func TestCandidateShape_HashIgnoresIncludeOrderOnly(t *testing.T) {
	a := mustShape(t, "CREATE INDEX i ON t (x) INCLUDE (a, b)")
	b := mustShape(t, "CREATE INDEX j ON public.t (x) INCLUDE (b, a)")
	c := mustShape(t, "CREATE INDEX i ON t (x) INCLUDE (a)")
	d := mustShape(t, "CREATE INDEX i ON t (x text_pattern_ops) INCLUDE (a, b)")
	if a.hash() != b.hash() {
		t.Fatalf("include order changed the hash: %s vs %s", a.hash(), b.hash())
	}
	if len(a.hash()) != 64 {
		t.Fatalf("hash %q is not 64 hex characters", a.hash())
	}
	if a.hash() == c.hash() || a.hash() == d.hash() || c.hash() == d.hash() {
		t.Fatal("different shapes share a hash")
	}
}

// The key list is the boundary for a separator collision: keys are a list,
// not a joined string, so ("a,b") and (a, b) never collide.
func TestCandidateShape_HashSeparatesKeysFromIncludes(t *testing.T) {
	a := mustShape(t, `CREATE INDEX i ON t ("a,b")`)
	b := mustShape(t, "CREATE INDEX i ON t (a, b)")
	c := mustShape(t, "CREATE INDEX i ON t (a) INCLUDE (b)")
	if a.hash() == b.hash() || b.hash() == c.hash() || a.sameIdea(b) {
		t.Fatalf("separator collision: %s %s %s", a.hash(), b.hash(), c.hash())
	}
}

func TestCandidateShape_InvalidInput(t *testing.T) {
	for _, ddl := range []string{
		"",
		"DROP INDEX i",
		"CREATE TABLE t (x int)",
		"CREATE INDEX i ON t ()",
		"CREATE INDEX i ON t (x",
		"CREATE INDEX i ON t (x) INCLUDE (a",
		"CREATE INDEX i ON t (x, )",
		"CREATE INDEX i ON t (x) WHERE",
	} {
		if s, err := shapeOfCandidate(ddl); err == nil {
			t.Errorf("shapeOfCandidate(%q) = %+v, want an error", ddl, s)
		}
	}
}

func TestCandidateShape_ZeroValueMatchesNothing(t *testing.T) {
	var zero candidateShape
	real := mustShape(t, "CREATE INDEX i ON t (x)")
	if zero.sameIdea(real) || real.sameIdea(zero) || zero.sameIdea(zero) {
		t.Fatal("a zero shape (no method, no keys) must never match")
	}
}

func TestCandidateShape_StringRendersEveryPart(t *testing.T) {
	s := mustShape(t, `CREATE INDEX i ON t USING btree (lower(email) DESC NULLS LAST, `+
		`"Code" COLLATE "C" text_pattern_ops) INCLUDE (b, a) WHERE (deleted_at IS NULL)`)
	want := `btree (lower(email) desc nulls last, "Code" collate "C" text_pattern_ops) ` +
		`INCLUDE (a, b) WHERE deleted_at is null`
	if got := s.String(); got != want {
		t.Fatalf("String() = %q\nwant        %q", got, want)
	}
}

// Removing spaces between operators must not fuse two tokens into a new
// one: "a - -1" must not become the comment marker "a--1".
func TestCandidateShape_OperatorsStayApart(t *testing.T) {
	s := mustShape(t, "CREATE INDEX i ON t (x) WHERE a - -1 > 0")
	if strings.Contains(s.Predicate, "--") {
		t.Fatalf("predicate %q fused two minus signs into a comment", s.Predicate)
	}
	if other := mustShape(t, "CREATE INDEX i ON t (x) WHERE a + 1 > 0"); s.sameIdea(other) {
		t.Fatal("a - -1 and a + 1 are different predicates as written")
	}
}
