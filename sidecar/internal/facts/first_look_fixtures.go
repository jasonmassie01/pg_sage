package facts

import (
	"fmt"
	"strings"
	"time"
)

// FirstLookMinIdle is the shortest statistics window over which the first
// look treats a test-named schema with no traffic as idle: the same quiet
// period the test-fixture detector waits for.
const FirstLookMinIdle = 6 * time.Hour

// SchemaActivity is one schema as the first look saw it: its tables and
// the scans plus row writes on them since the statistics window began.
type SchemaActivity struct {
	Name     string
	Tables   int
	Activity int64
}

// IdleFixtureProposals proposes the test-named schemas with no traffic
// over a statistics window of idleFor, at least FirstLookMinIdle. Subjects
// match the test-fixture detector's (the same family rule): a family of two
// or more schemas as one pattern once every test-named schema it would
// match is idle, else each idle schema by name.
// They are proposals: nothing binds until an operator confirms them.
func IdleFixtureProposals(schemas []SchemaActivity, idleFor time.Duration,
	now time.Time) []Proposal {
	if idleFor < FirstLookMinIdle {
		return nil
	}
	return fixtureProposals(schemas, fixtureRule[SchemaActivity]{
		name:  func(s SchemaActivity) string { return s.Name },
		quiet: func(s SchemaActivity) bool { return s.Activity == 0 },
		propose: func(subject string, members []SchemaActivity) Proposal {
			return idleProposal(subject, members, idleFor, now)
		},
	})
}

func idleProposal(subject string, members []SchemaActivity, idleFor time.Duration,
	now time.Time) Proposal {
	tables := 0
	names := make([]string, len(members))
	for i, m := range members {
		tables += m.Tables
		names[i] = m.Name
	}
	if len(names) > 5 {
		names = append(names[:5], "...")
	}
	detail := fmt.Sprintf("%d schemas %s (e.g. %s), %d tables: no scans or writes since "+
		"the statistics window began %s ago", len(members), subject, members[0].Name,
		tables, idleFor.Round(time.Minute))
	if len(members) == 1 {
		detail = fmt.Sprintf("schema %s: %d tables, no scans or writes since the "+
			"statistics window began %s ago", members[0].Name, tables,
			idleFor.Round(time.Minute))
	}
	return Proposal{Type: TypeTestFixture, Kind: KindSchema, Subject: subject,
		Source: SourceDetector, ProposedBy: "detector:first_look",
		Rationale: "Test-named schemas with no traffic in the statistics window: " +
			"leftovers of test runs, not application workload (" +
			strings.Join(names, ", ") + ").",
		Evidence: []Citation{{Kind: "catalog", Ref: "schemas:" + subject, Detail: detail,
			ObservedAt: now}}}
}
