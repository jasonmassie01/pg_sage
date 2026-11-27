package firstlook

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// The degraded sequence check hands out the shared rolegrants SQL for the
// role it ran as: a GRANT that runs as printed, with no placeholder, and
// after it the check is no longer degraded for that schema.
func TestUnreadableSequenceNoteNamesTheRole(t *testing.T) {
	admin, ctx := livePool(t)
	s := seedProblems(t, ctx, admin)
	mon := monitorRole(t, ctx, admin)
	var role string
	if err := mon.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatalf("role: %v", err)
	}
	r, err := Run(ctx, mon, testOptions("fl_seqgrant_"+s))
	if err != nil {
		t.Fatalf("run as monitor: %v", err)
	}
	c := checkStatus(r, RuleSequenceRunway)
	if c.Status != CheckDegraded || strings.Contains(c.Note, "<") ||
		!strings.Contains(c.Note, " TO "+sqlident.Role(role)+";") ||
		!strings.Contains(c.Note, sqlident.Ident(s)) {
		t.Fatalf("sequence check = %+v, want the GRANT on %s to %s", c, s, role)
	}
	execAll(t, ctx, admin, "GRANT SELECT ON ALL SEQUENCES IN SCHEMA "+
		pgx.Identifier{s}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
	r, err = Run(ctx, mon, testOptions("fl_seqgrant_"+s))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if c := checkStatus(r, RuleSequenceRunway); strings.Contains(c.Note,
		sqlident.Ident(s)+" ") || strings.Contains(c.Note, sqlident.Ident(s)+",") {
		t.Fatalf("after grant the note still names %s: %+v", s, c)
	}
}
