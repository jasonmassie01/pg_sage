package firstlook

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// No concurrent access tests: fixtureProposals is a pure function over the
// schemas it is given and keeps no state.

// idleSchemas are n idle test schemas of the ci_ family in name order,
// followed by the extra schemas given (which must sort after them).
func idleSchemas(n int, extra ...Schema) []Schema {
	var ss []Schema
	for i := 0; i < n; i++ {
		ss = append(ss, Schema{Name: fmt.Sprintf("ci_%04d", i), Tables: 1})
	}
	return append(ss, extra...)
}

func proposalSubjects(ss []Schema, w StatsWindow, now time.Time) []string {
	out := []string{}
	for _, p := range fixtureProposals(ss, w, now) {
		out = append(out, p.Subject)
	}
	sort.Strings(out)
	return out
}

func TestFixtureProposalsHoldTheFamilyCutByTheRowCap(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	w := StatsWindow{Since: now.Add(-48 * time.Hour), Known: true}
	tail := []Schema{{Name: "test_memory_a1b2", Tables: 1}, {Name: "test_memory_c3d4", Tables: 1}}
	for _, tc := range []struct {
		desc string
		ss   []Schema
		want []string
	}{
		// Under the cap the read is complete: both families are proposed.
		{"one under the cap", idleSchemas(maxSchemaRows-3, tail...),
			[]string{"ci_*", "test_memory_*"}},
		// At the cap a live test_memory_live may sort past the cut: the family
		// the last row belongs to waits; ci_* lies wholly before the cut.
		{"at the cap", idleSchemas(maxSchemaRows-2, tail...), []string{"ci_*"}},
		// A lone last row is not proposed either: past the cut is unknown.
		{"at the cap, lone last row",
			idleSchemas(maxSchemaRows-1, Schema{Name: "tmp_last", Tables: 1}),
			[]string{"ci_*"}},
		{"no schemas", nil, []string{}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := proposalSubjects(tc.ss, w, now)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("subjects = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFixtureProposalsNeedAKnownWindow(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ss := idleSchemas(3)
	if got := fixtureProposals(ss, StatsWindow{Since: now.Add(-48 * time.Hour)}, now); got != nil {
		t.Fatalf("an unknown window gave %+v", got)
	}
	known := StatsWindow{Since: now.Add(-48 * time.Hour), Known: true}
	if got := proposalSubjects(ss, known, now); fmt.Sprint(got) != "[ci_*]" {
		t.Fatalf("a known window gave %q, want [ci_*]", got)
	}
	if ss[2].Activity != 0 {
		t.Fatalf("fixtureProposals changed its input: %+v", ss[2])
	}
}
