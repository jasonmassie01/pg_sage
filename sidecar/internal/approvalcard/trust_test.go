package approvalcard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
)

// A card shows the trust of its action class on its database (roadmap
// 1.2): the level, the evidence counts and the path to the next level, as
// one line in chat and as the card's trust field. It informs the decision
// and is not part of the content a decision is bound to.

func analyzeTrustRow() *earned.TrustRow {
	l2 := earned.L2
	eta := now.Add(3 * time.Hour)
	return &earned.TrustRow{Family: earned.FamilyHygiene, Kind: earned.KindSelfInitiated,
		Class: earned.ClassAnalyze, Level: earned.L2, Effective: &l2, Cap: earned.L3,
		Provenance: "grandfathered",
		Evidence: earned.TrustCounts{Improved: 2, Neutral: 3, Regressed: 0, RolledBack: 0,
			Rejected: 1},
		Next: &earned.Assessment{Target: earned.L3, Checks: []earned.Check{
			{Name: "class_cap", Met: true},
			{Name: "class_successes", Met: false, Observed: "5", Required: "10",
				How: "5 more verified successes"},
			{Name: "observation_floor", Met: false, Observed: "9h", Required: "12h",
				ETA: &eta},
		}}}
}

func analyzeInputs() Inputs {
	a := queued(77, "analyze_table", "ANALYZE public.orders", "", "safe")
	return Inputs{Database: "orders", Action: a, Now: now}
}

func TestCardShowsTheClassTrust(t *testing.T) {
	in := analyzeInputs()
	in.Trust = analyzeTrustRow()
	c := Assemble(in)
	tr := c.Trust
	if tr == nil || tr.Family != "hygiene" || tr.Class != "analyze" || tr.Level != "L2" ||
		tr.Effective != "L2" || tr.Cap != "L3" || tr.NextLevel != "L3" ||
		tr.Provenance != "grandfathered" || tr.Evidence.Improved != 2 ||
		tr.Evidence.Neutral != 3 || tr.Evidence.Rejected != 1 {
		t.Fatalf("trust = %+v", tr)
	}
	if len(tr.Path) != 2 || !strings.Contains(tr.Path[0], "5 more verified successes") ||
		!strings.Contains(tr.Path[1], "observation_floor") {
		t.Fatalf("path = %q", tr.Path)
	}
	for _, want := range []string{"hygiene/analyze", "L2", "2 improved", "3 neutral",
		"0 regressed", "1 rejected", "next L3", "5 more verified successes"} {
		if !strings.Contains(tr.Line, want) {
			t.Fatalf("line %q lacks %q", tr.Line, want)
		}
	}
	text := Text(c, now)
	if n := strings.Count(text, "\nTrust: "); n != 1 || !strings.Contains(text, tr.Line) {
		t.Fatalf("chat text has %d trust lines:\n%s", n, text)
	}
	without := Assemble(analyzeInputs())
	if without.CardHash != c.CardHash {
		t.Fatal("the trust changed the content hash a decision is bound to")
	}
}

func TestCardTrustAtTheCapAndWithoutALedger(t *testing.T) {
	in := analyzeInputs()
	row := analyzeTrustRow()
	row.Level, row.Next = earned.L3, nil
	in.Trust = row
	c := Assemble(in)
	if c.Trust == nil || c.Trust.NextLevel != "" || len(c.Trust.Path) != 0 ||
		!strings.Contains(c.Trust.Line, "highest level") {
		t.Fatalf("trust at the cap = %+v", c.Trust)
	}
	plain := Assemble(analyzeInputs())
	if plain.Trust != nil || strings.Contains(Text(plain, now), "\nTrust: ") {
		t.Fatalf("a card without a ledger shows trust: %+v", plain.Trust)
	}
}

func TestCardTrustUnavailableIsShown(t *testing.T) {
	in := analyzeInputs()
	in.TrustErr = errors.New("read class record: connection refused")
	c := Assemble(in)
	if c.Trust == nil || !strings.Contains(c.Trust.Unavailable, "connection refused") ||
		!strings.Contains(c.Trust.Line, "unavailable") || c.Trust.Level != "" {
		t.Fatalf("unavailable trust = %+v", c.Trust)
	}
	if !strings.Contains(Text(c, now), "Trust: unavailable") {
		t.Fatalf("chat text:\n%s", Text(c, now))
	}
}

func TestTrustRowForPicksTheActionsPair(t *testing.T) {
	v := earned.TrustView{Rows: []earned.TrustRow{
		{Family: earned.FamilyTuning, Class: earned.ClassIndexCreate, Level: earned.L1},
		*analyzeTrustRow(),
	}}
	row := trustRowFor(v, queued(5, "analyze_table", "ANALYZE public.orders", "", "safe"))
	if row == nil || row.Class != earned.ClassAnalyze {
		t.Fatalf("row = %+v", row)
	}
	if row := trustRowFor(v, queued(6, "cancel_backend", "SELECT pg_cancel_backend(1)",
		"", "safe")); row != nil {
		t.Fatalf("an unclassified action got a trust row: %+v", row)
	}
	a := queued(7, "vacuum_table", "VACUUM public.orders", "", "safe")
	if row := trustRowFor(v, a); row != nil {
		t.Fatalf("a pair missing from the view got a row: %+v", row)
	}
}
