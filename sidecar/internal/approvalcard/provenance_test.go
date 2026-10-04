package approvalcard

import (
	"strings"
	"testing"
)

// Owner decision (2026-10-04): an approval card shows how its item was
// proposed and by whom (Ask Sage on a user's question), as a typed field
// and in the card text, not only in the title. pg_sage's own items have
// none.
func TestAssembleShowsTheProposalOrigin(t *testing.T) {
	a := queued(9, "analyze_table", "ANALYZE public.t", "", "safe")
	a.ProposedVia, a.ProposedBy = "ask_sage", "user:42"
	c := Assemble(Inputs{Database: "orders", Action: a, Now: now})
	if c.Origin == nil || c.Origin.Via != "ask_sage" || c.Origin.By != "user:42" ||
		c.Origin.Label != "Ask Sage" {
		t.Fatalf("origin = %+v", c.Origin)
	}
	if text := Text(c, now); !strings.Contains(text, "Proposed via Ask Sage by user:42") {
		t.Fatalf("card text lacks the origin:\n%s", text)
	}
	own := Assemble(Inputs{Database: "orders", Action: queued(10, "analyze_table",
		"ANALYZE public.t", "", "safe"), Now: now})
	if own.Origin != nil || strings.Contains(Text(own, now), "Proposed via") {
		t.Fatalf("pg_sage's own item shows an origin: %+v", own.Origin)
	}
}
