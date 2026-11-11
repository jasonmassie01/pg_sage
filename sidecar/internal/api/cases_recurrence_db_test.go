package api

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// A recurring incident's case links to its resolved predecessor and
// counts the occurrences of its chain, read in one batched lookup for all
// of a database's incident cases (cases-07). The case identity keeps the
// incident UUID.

const (
	recurFirst  = "00000000-0000-0000-0000-00000000c701"
	recurSecond = "00000000-0000-0000-0000-00000000c702"
	recurThird  = "00000000-0000-0000-0000-00000000c703"
	recurOther  = "00000000-0000-0000-0000-00000000c704"
	recurAgain  = "00000000-0000-0000-0000-00000000c705"
	recurLone   = "00000000-0000-0000-0000-00000000c706"
)

func seedRecurrenceChain(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.incidents
		(id, detected_at, severity, root_cause, source, database_name, resolved_at,
		 previous_incident_id)
		VALUES
		($1, '2026-10-20T00:00:00Z', 'warning', 'lock chain', 'deterministic', 'prod',
		 '2026-10-21T00:00:00Z', NULL),
		($2, '2026-11-01T00:00:00Z', 'warning', 'lock chain', 'deterministic', 'prod',
		 '2026-11-02T00:00:00Z', $1),
		($3, '2026-11-09T00:00:00Z', 'warning', 'lock chain', 'deterministic', 'prod',
		 NULL, $2),
		($4, '2026-10-01T00:00:00Z', 'critical', 'xid', 'deterministic', 'prod',
		 '2026-10-05T00:00:00Z', NULL),
		($5, '2026-11-08T00:00:00Z', 'critical', 'xid', 'deterministic', 'prod',
		 NULL, $4),
		($6, '2026-11-08T00:00:00Z', 'info', 'lone', 'deterministic', 'prod',
		 NULL, NULL)`,
		recurFirst, recurSecond, recurThird, recurOther, recurAgain, recurLone)
	if err != nil {
		t.Fatalf("seed incident chain: %v", err)
	}
}

func casesBySource(list []cases.Case) map[string]cases.Case {
	out := make(map[string]cases.Case, len(list))
	for _, c := range list {
		out[c.SourceIDs[0]] = c
	}
	return out
}

func wantRecurrence(t *testing.T, c cases.Case, prev string, count int, resolved string) {
	t.Helper()
	r := c.Recurrence
	if r == nil {
		t.Fatalf("case %s has no recurrence", c.ID)
	}
	at, _ := time.Parse(time.RFC3339, resolved)
	if r.PreviousID != prev || r.Count != count || r.PreviousResolvedAt == nil ||
		!r.PreviousResolvedAt.Equal(at) || r.HistoryUnavailable {
		t.Fatalf("case %s recurrence = %+v (resolved %v), want %s x%d resolved %s",
			c.ID, r, r.PreviousResolvedAt, prev, count, resolved)
	}
}

func TestIncidentCases_RecurrenceLinksTheResolvedPredecessor(t *testing.T) {
	pool, rec, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	seedRecurrenceChain(t, ctx)

	rec.Reset()
	list, cut, err := queryIncidentCases(ctx, namedPool{name: "prod", pool: pool})
	if err != nil || cut {
		t.Fatalf("queryIncidentCases: cut=%v err=%v", cut, err)
	}
	if n := rec.RoundTrips(); n != 2 {
		t.Fatalf("round trips = %d (%v), want the incident read and one batched lookup",
			n, rec.Matching(""))
	}
	got := casesBySource(list)
	if len(got) != 3 {
		t.Fatalf("open incident cases = %d, want 3", len(got))
	}
	wantRecurrence(t, got[recurAgain], recurOther, 2, "2026-10-05T00:00:00Z")
	wantRecurrence(t, got[recurThird], recurSecond, 3, "2026-11-02T00:00:00Z")
	if got[recurLone].Recurrence != nil {
		t.Fatalf("a first occurrence has recurrence %+v", got[recurLone].Recurrence)
	}
	if id := got[recurThird].ID; id != "incident:prod:deterministic:"+recurThird {
		t.Fatalf("case identity = %q, want the incident UUID kept", id)
	}
}

// A cycle in previous_incident_id (only reachable by hand-edited rows)
// must end the walk rather than loop.
func TestIncidentCases_RecurrenceCycleTerminates(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	seedRecurrenceChain(t, ctx)
	if _, err := pool.Exec(ctx, `UPDATE sage.incidents SET previous_incident_id = $1
		WHERE id = $2`, recurThird, recurFirst); err != nil {
		t.Fatalf("make cycle: %v", err)
	}
	list, _, err := queryIncidentCases(ctx, namedPool{name: "prod", pool: pool})
	if err != nil {
		t.Fatalf("queryIncidentCases: %v", err)
	}
	wantRecurrence(t, casesBySource(list)[recurThird], recurSecond, 3, "2026-11-02T00:00:00Z")
}

// A failed lookup keeps the link it already has and says the history is
// unavailable, rather than reporting a first occurrence.
func TestIncidentCases_RecurrenceLookupFailureIsDisclosed(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	list := []cases.Case{{ID: "incident:prod:deterministic:" + recurSecond,
		SourceType: cases.SourceIncidentType, SourceIDs: []string{recurSecond},
		Recurrence: &cases.Recurrence{PreviousID: recurFirst}}}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	enrichIncidentRecurrence(cancelled, pool, list)
	r := list[0].Recurrence
	if r == nil || r.PreviousID != recurFirst || !r.HistoryUnavailable || r.Count != 0 {
		t.Fatalf("recurrence after failed lookup = %+v", r)
	}
}

// A predecessor row that is gone leaves a chain of one: the case keeps
// its link and says the history is unavailable, never a first occurrence.
// The foreign key normally prevents this, so the fixture bypasses it.
func TestIncidentCases_RecurrenceMissingPredecessorIsDisclosed(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	seedRecurrenceChain(t, ctx)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("bypass the foreign key: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.incidents SET previous_incident_id = $1
		WHERE id = $2`, "00000000-0000-0000-0000-00000000c7ff", recurLone); err != nil {
		t.Fatalf("point at a missing predecessor: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	list, _, err := queryIncidentCases(ctx, namedPool{name: "prod", pool: pool})
	if err != nil {
		t.Fatalf("queryIncidentCases: %v", err)
	}
	got := casesBySource(list)
	r := got[recurLone].Recurrence
	if r == nil || r.PreviousID != "00000000-0000-0000-0000-00000000c7ff" ||
		!r.HistoryUnavailable || r.Count != 0 || r.PreviousResolvedAt != nil {
		t.Fatalf("recurrence with a missing predecessor = %+v", r)
	}
	wantRecurrence(t, got[recurThird], recurSecond, 3, "2026-11-02T00:00:00Z")
}
