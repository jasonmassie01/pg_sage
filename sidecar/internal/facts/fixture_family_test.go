package facts

import (
	"sort"
	"testing"
	"time"
)

// No concurrent access tests: the family rule is a pure function over its
// inputs and the detector's tracker is not involved.

type fixtureCase struct {
	name string
	live bool
}

// detectorSubjects runs the detector's proposal rule over the schemas,
// with idle ones quiet past the detector's period and live ones fresh.
func detectorSubjects(cases []fixtureCase, now time.Time) []string {
	d := &fixtureDetector{quiet: time.Hour, now: func() time.Time { return now }}
	var schemas []testSchema
	for _, c := range cases {
		s := testSchema{name: c.name, tables: 1, quiet: 2 * time.Hour}
		if c.live {
			s.quiet = 0
		}
		schemas = append(schemas, s)
	}
	return subjectsOf(d.proposalsFor(schemas))
}

// firstLookSubjects runs the first look's rule over the same schemas.
func firstLookSubjects(cases []fixtureCase, now time.Time) []string {
	var acts []SchemaActivity
	for _, c := range cases {
		a := SchemaActivity{Name: c.name, Tables: 1}
		if c.live {
			a.Activity = 7
		}
		acts = append(acts, a)
	}
	return subjectsOf(IdleFixtureProposals(acts, 24*time.Hour, now))
}

func subjectsOf(props []Proposal) []string {
	out := []string{}
	for _, p := range props {
		out = append(out, p.Subject)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFixtureSubjectsAgreeAcrossDetectorAndFirstLook(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		desc  string
		cases []fixtureCase
		want  []string
	}{
		{"idle family and a lone schema",
			[]fixtureCase{{"test_run_1234", false}, {"test_run_5678", false},
				{"ci_lonely", false}},
			[]string{"ci_lonely", "test_run_*"}},
		{"family with a live member waits",
			[]fixtureCase{{"test_run_1234", false}, {"test_run_5678", true}},
			[]string{}},
		{"live non-member sharing the prefix holds the family",
			[]fixtureCase{{"test_memory_a1b2", false}, {"test_memory_c3d4", false},
				{"test_memory_live", true}},
			[]string{}},
		{"idle non-member sharing the prefix does not hold it",
			[]fixtureCase{{"test_memory_a1b2", false}, {"test_memory_c3d4", false},
				{"test_memory_idle", false}},
			[]string{"test_memory_*", "test_memory_idle"}},
		{"single suffixed schema is proposed by name",
			[]fixtureCase{{"test_memory_a1b2", false}, {"tmp_live", true}},
			[]string{"test_memory_a1b2"}},
		{"mixed-case dash family",
			[]fixtureCase{{"Tmp-Run-9f8e", false}, {"Tmp-Run-7d6c", false}},
			[]string{`"Tmp-Run-*"`}},
		{"no schemas", nil, []string{}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			det, fl := detectorSubjects(tc.cases, now), firstLookSubjects(tc.cases, now)
			if !equalStrings(det, tc.want) {
				t.Errorf("detector subjects = %q, want %q", det, tc.want)
			}
			if !equalStrings(fl, tc.want) {
				t.Errorf("first look subjects = %q, want %q", fl, tc.want)
			}
		})
	}
}

// The detector's quiet threshold is inclusive and a zero quiet (never
// observed quiet) is live, at the family and the single level alike.
func TestDetectorFamilyQuietBoundary(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	d := &fixtureDetector{quiet: time.Hour, now: func() time.Time { return now }}
	at := []testSchema{{name: "test_run_1234", tables: 1, quiet: time.Hour},
		{name: "test_run_5678", tables: 2, quiet: 3 * time.Hour}}
	got := d.proposalsFor(at)
	if len(got) != 1 || got[0].Subject != "test_run_*" {
		t.Fatalf("family quiet exactly at the period gave %+v", got)
	}
	if got[0].Evidence[0].Detail !=
		"2 schemas test_run_* (e.g. test_run_1234), 3 tables, no scans or writes for 1h0m0s" {
		t.Fatalf("detail = %q", got[0].Evidence[0].Detail)
	}
	just := []testSchema{{name: "test_run_1234", tables: 1, quiet: time.Hour - time.Second},
		{name: "test_run_5678", tables: 1, quiet: 3 * time.Hour}}
	if got := d.proposalsFor(just); len(got) != 0 {
		t.Fatalf("a member just under the period gave %+v", got)
	}
	if got := d.proposalsFor([]testSchema{{name: "ci_x", quiet: 0}}); len(got) != 0 {
		t.Fatalf("a never-quiet schema gave %+v", got)
	}
	if got := d.proposalsFor(nil); len(got) != 0 {
		t.Fatalf("no schemas gave %+v", got)
	}
}
