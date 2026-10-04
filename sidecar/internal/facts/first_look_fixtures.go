package facts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// FirstLookMinIdle is the shortest statistics window over which the first
// look treats a test-named schema with no traffic as idle: the same quiet
// period the test-fixture detector waits for.
const FirstLookMinIdle = 6 * time.Hour

var testSchemaName = regexp.MustCompile(`(?i)` + testSchemaRegex)

// TestSchemaPattern is the PostgreSQL regular expression (case
// insensitive) that names test schemas.
func TestSchemaPattern() string { return testSchemaRegex }

// IsTestSchemaName reports whether name looks like a test schema.
func IsTestSchemaName(name string) bool { return testSchemaName.MatchString(name) }

// SchemaActivity is one schema as the first look saw it: its tables and
// the scans plus row writes on them since the statistics window began.
type SchemaActivity struct {
	Name     string
	Tables   int
	Activity int64
}

// IdleFixtureProposals proposes the test-named schemas with no traffic
// over a statistics window of idleFor, at least FirstLookMinIdle. Subjects
// match the test-fixture detector's: a family of two or more schemas as
// one pattern once every member is idle, else each idle schema by name.
// They are proposals: nothing binds until an operator confirms them.
func IdleFixtureProposals(schemas []SchemaActivity, idleFor time.Duration,
	now time.Time) []Proposal {
	if idleFor < FirstLookMinIdle {
		return nil
	}
	groups := map[string][]SchemaActivity{}
	var keys []string
	for _, s := range schemas {
		if !IsTestSchemaName(s.Name) {
			continue
		}
		key := familyKey(s.Name)
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], s)
	}
	sort.Strings(keys)
	var out []Proposal
	for _, key := range keys {
		out = append(out, idleProposals(key, groups[key], idleFor, now)...)
	}
	return out
}

func idleProposals(key string, members []SchemaActivity, idleFor time.Duration,
	now time.Time) []Proposal {
	if key != "" && len(members) >= 2 {
		for _, m := range members {
			if m.Activity != 0 {
				return nil // a family with a live member waits
			}
		}
		return []Proposal{idleProposal(quoteIdent(key+"*"), members, idleFor, now)}
	}
	var out []Proposal
	for _, m := range members {
		if m.Activity == 0 {
			out = append(out, idleProposal(quoteIdent(m.Name), []SchemaActivity{m},
				idleFor, now))
		}
	}
	return out
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
