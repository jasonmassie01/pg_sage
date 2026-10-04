package facts

import (
	"fmt"
	"strings"
)

// SourceFix is what pg_sage hands over instead of acting when a confirmed
// fact binds a change: which fact, why, and for an object owned by the
// application's migrations the migration it recommends ("route to a PR
// instead of DDL").
type SourceFix struct {
	FactID     int64  `json:"fact_id"`
	Fact       string `json:"fact"`
	Provenance string `json:"provenance"`
	Object     string `json:"object"`
	Route      string `json:"route"`
	Summary    string `json:"summary"`
	// Migration is the change to add to the application's migrations; Down
	// undoes it. Both are text for a person or a coding agent, never run by
	// pg_sage.
	Migration string `json:"migration,omitempty"`
	Down      string `json:"down,omitempty"`
}

// BuildSourceFix describes the redirect of one binding of a change (sql,
// rollback) to its route.
func BuildSourceFix(b Binding, sql, rollback string) SourceFix {
	fix := SourceFix{FactID: b.Fact.ID, Fact: b.Fact.Describe(),
		Provenance: b.Fact.Provenance(), Object: b.Object, Route: string(b.Route)}
	why := fmt.Sprintf("%s (%s)", fix.Fact, fix.Provenance)
	switch b.Route {
	case RouteSourceFix:
		fix.Summary = "pg_sage will not run this DDL: " + why + ". Add the change to " +
			"the application's migrations instead."
		fix.Migration = migrationText(b, sql)
		if strings.TrimSpace(rollback) != "" {
			fix.Down = statement(rollback)
		}
	case RouteKeep:
		fix.Summary = "pg_sage will not run this: " + why + ". Its data and indexes " +
			"are kept."
	case RouteAlert:
		fix.Summary = "pg_sage will not run this: " + why + ". pg_sage alerts instead of " +
			"acting on the slot."
	case RouteExcluded:
		fix.Summary = "pg_sage leaves this alone: " + why + ". A test fixture is not " +
			"workload; clean up the fixtures with the cleanup batch."
	default:
		fix.Summary = "pg_sage holds this: " + why + ". The table's window decides " +
			"when work may run."
	}
	return fix
}

func migrationText(b Binding, sql string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("-- pg_sage source-fix packet (%s): %s",
		b.Fact.Provenance(), b.Fact.Describe()),
		"-- Add this change to the application's migrations; pg_sage will not run it.")
	if strings.Contains(strings.ToUpper(sql), "CONCURRENTLY") {
		lines = append(lines, "-- CONCURRENTLY cannot run inside a transaction: make "+
			"this migration non-transactional.")
	}
	lines = append(lines, statement(sql))
	return strings.Join(lines, "\n") + "\n"
}

// statement ends sql with exactly one semicolon.
func statement(sql string) string {
	return strings.TrimRight(strings.TrimSpace(sql), "; \t\n") + ";"
}
