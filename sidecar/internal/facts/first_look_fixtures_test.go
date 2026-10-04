package facts

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: IdleFixtureProposals is a pure function.

func TestIdleFixtureProposals(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	schemas := []SchemaActivity{
		{Name: "test_run_1234", Tables: 2},
		{Name: "test_run_5678", Tables: 1},
		{Name: "tmp_live", Tables: 1, Activity: 5},
		{Name: "ci_lonely", Tables: 3},
	}
	props := IdleFixtureProposals(schemas, 24*time.Hour, now)
	if len(props) != 2 {
		t.Fatalf("proposals = %+v, want the test_run_ family and ci_lonely", props)
	}
	subjects := map[string]bool{}
	for _, p := range props {
		subjects[p.Subject] = true
		if p.Type != TypeTestFixture || p.Kind != KindSchema || p.Source != SourceDetector {
			t.Fatalf("proposal %+v has the wrong type, kind or source", p)
		}
		if _, err := Validate(p); err != nil {
			t.Fatalf("proposal %+v is invalid: %v", p, err)
		}
		if len(p.Evidence) == 0 || !strings.Contains(p.Evidence[0].Detail,
			"since the statistics window began") {
			t.Fatalf("proposal %+v does not cite the statistics window", p)
		}
		if p.ProposedBy != "detector:first_look" {
			t.Fatalf("proposed_by = %q", p.ProposedBy)
		}
	}
	if !subjects["test_run_*"] || !subjects["ci_lonely"] {
		t.Fatalf("subjects = %v, want the family pattern the worker uses and ci_lonely",
			subjects)
	}
}

func TestIdleFixtureProposalsNeedALongEnoughWindow(t *testing.T) {
	now := time.Now()
	schemas := []SchemaActivity{{Name: "test_a", Tables: 1}}
	if got := IdleFixtureProposals(schemas, FirstLookMinIdle-time.Second, now); len(got) != 0 {
		t.Fatalf("short window gave %+v", got)
	}
	if got := IdleFixtureProposals(schemas, FirstLookMinIdle, now); len(got) != 1 {
		t.Fatalf("window at the minimum gave %+v", got)
	}
	if got := IdleFixtureProposals(nil, 48*time.Hour, now); len(got) != 0 {
		t.Fatalf("nil schemas gave %+v", got)
	}
}

func TestIdleFixtureProposalsFamilyWithLiveMemberWaits(t *testing.T) {
	schemas := []SchemaActivity{{Name: "test_run_1234", Tables: 1},
		{Name: "test_run_5678", Tables: 1, Activity: 1}}
	if got := IdleFixtureProposals(schemas, 48*time.Hour, time.Now()); len(got) != 0 {
		t.Fatalf("a family with a live member gave %+v", got)
	}
}

func TestIdleFixtureProposalsSkipNonTestNames(t *testing.T) {
	schemas := []SchemaActivity{{Name: "public", Tables: 10}, {Name: "sage", Tables: 30},
		{Name: "billing", Tables: 3}}
	if got := IdleFixtureProposals(schemas, 48*time.Hour, time.Now()); len(got) != 0 {
		t.Fatalf("non-test schemas gave %+v", got)
	}
	if !IsTestSchemaName("pytest_abc") || IsTestSchemaName("contest") {
		t.Fatal("IsTestSchemaName disagrees with the detector's pattern")
	}
}
