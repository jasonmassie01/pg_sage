package perfgate

import (
	"fmt"
	"strings"
	"testing"
)

// sage.shadow_decision has four update paths: the seen bump and the
// insert's ON CONFLICT bump (last_seen_at, seen_count), markApplied
// (applied_after, applied_detected_at) and the score write (status,
// score, counted, scored_at). Only the score write moves a row between
// the partial indexes it is read through, so only it cannot be HOT. Gate
// F subtracts the rows of the statement carrying the exemption's tag from
// the table's updates and charges the rest like any other table.

const scoreWriteQuery = "/* pg_sage shadow:score v1 */ UPDATE sage.shadow_decision\n" +
	"\tSET status = 'scored', score = $2 WHERE id = $1 AND status = 'pending'"

func hotPhase(tables ...TableDelta) Phase {
	p := steadyPhase()
	p.Tables = tables
	return p
}

func scoreWrite(rows int64) Statement {
	return Statement{QueryID: 31, Query: scoreWriteQuery, Calls: rows, Rows: rows,
		TotalMs: 3, MeanMs: 0.1, MaxMs: 1}
}

func shadowTable(updates, hot int64) TableDelta {
	return TableDelta{Name: "sage.shadow_decision", Updates: updates, HotUpdates: hot,
		RowsWritten: updates}
}

func hotGates(t *testing.T, b Budgets, p Phase) []Offender {
	t.Helper()
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var out []Offender
	for _, o := range got {
		if o.Gate == GateHotUpdates {
			out = append(out, o)
		}
	}
	return out
}

func TestDefaultHotExemptionIsTheScoreWriteOnly(t *testing.T) {
	b := DefaultBudgets()
	if len(b.HotExempt) != 1 {
		t.Fatalf("HOT exemptions = %v, want shadow_decision's score write only", b.HotExempt)
	}
	ex, ok := b.HotExempt["sage.shadow_decision"]
	if !ok || ex.Tag != "shadow:score" || !strings.Contains(ex.Reason, "partial") ||
		!strings.Contains(ex.Reason, "seen") || !strings.Contains(ex.Reason, "charged") {
		t.Fatalf("shadow_decision exemption = %+v %t, want the shadow:score tag and a reason "+
			"that says the other update paths are charged", ex, ok)
	}
	for table, e := range b.HotExempt {
		if !strings.HasPrefix(table, "sage.") || !strings.Contains(e.Tag, ":") ||
			len(e.Reason) < 40 {
			t.Fatalf("exemption %q = %+v: a sage table, a statement tag and a reason, please",
				table, e)
		}
	}
}

// The score writes are subtracted; what is left is judged as usual.
func TestHotGateSubtractsOnlyTheTaggedScoreWrites(t *testing.T) {
	b := DefaultBudgets()
	cases := []struct {
		name     string
		table    TableDelta
		stmts    []Statement
		charged  bool
		measured float64
	}{
		{"only score writes", shadowTable(31, 0), []Statement{scoreWrite(31)}, false, 0},
		{"seen bumps HOT", shadowTable(41, 10), []Statement{scoreWrite(31)}, false, 0},
		{"seen bumps not HOT", shadowTable(41, 0), []Statement{scoreWrite(31)}, true, 0},
		{"exactly half HOT", shadowTable(41, 5), []Statement{scoreWrite(31)}, false, 0},
		{"just under half HOT", shadowTable(42, 5), []Statement{scoreWrite(31)}, true,
			500.0 / 11},
		{"exactly the minimum left, none HOT", shadowTable(36, 0),
			[]Statement{scoreWrite(31)}, true, 0},
		{"one under the minimum left", shadowTable(35, 0), []Statement{scoreWrite(31)},
			false, 0},
		{"score writes in two entries", shadowTable(41, 0),
			[]Statement{scoreWrite(20), scoreWrite(11)}, true, 0},
		{"more score rows than updates", shadowTable(20, 0), []Statement{scoreWrite(31)},
			false, 0},
	}
	for _, c := range cases {
		p := hotPhase(c.table)
		p.Statements = c.stmts
		got := hotGates(t, b, p)
		if !c.charged {
			if len(got) != 0 {
				t.Fatalf("%s: %+v, want no gate F offender", c.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Subject != "sage.shadow_decision" ||
			fmt.Sprintf("%.4f", got[0].Measured) != fmt.Sprintf("%.4f", c.measured) ||
			!strings.Contains(got[0].Detail, "shadow:score") {
			t.Fatalf("%s: %+v, want shadow_decision at %.1f%% HOT with the score writes "+
				"named", c.name, got, c.measured)
		}
	}
}

// Negative cases: an untagged update, or the tag on another table's
// statement, exempts nothing; a table without an exemption is charged in
// full even when a tagged statement ran.
func TestHotGateNeedsTheTaggedStatementOnTheExemptTable(t *testing.T) {
	b := DefaultBudgets()
	untagged := Statement{QueryID: 32, Calls: 31, Rows: 31, Query: "/* pg_sage */ UPDATE " +
		"sage.shadow_decision SET status = 'scored' WHERE id = $1"}
	p := hotPhase(shadowTable(31, 0))
	p.Statements = []Statement{untagged}
	if got := hotGates(t, b, p); len(got) != 1 || got[0].Measured != 0 {
		t.Fatalf("untagged score writes: %+v, want shadow_decision charged at 0%%", got)
	}
	p = hotPhase(shadowTable(31, 0), TableDelta{Name: "sage.findings", Updates: 31})
	p.Statements = []Statement{scoreWrite(31)}
	got := hotGates(t, b, p)
	if len(got) != 1 || got[0].Subject != "sage.findings" {
		t.Fatalf("offenders = %+v, want only sage.findings", got)
	}
}

// The exemption is a budget, not a hard-coded skip: without it the table
// is charged like any other.
func TestHotGateChargesAnExemptTableWhenTheExemptionIsRemoved(t *testing.T) {
	b := DefaultBudgets()
	b.HotExempt = nil
	p := hotPhase(shadowTable(31, 0))
	p.Statements = []Statement{scoreWrite(31)}
	got := hotGates(t, b, p)
	if len(got) != 1 || got[0].Subject != "sage.shadow_decision" {
		t.Fatalf("offenders = %+v, want shadow_decision charged", got)
	}
}

// Invalid input: an exemption needs a statement tag and a reason.
func TestHotExemptionValidation(t *testing.T) {
	for name, ex := range map[string]HotExemption{
		"no tag":    {Reason: strings.Repeat("why ", 12)},
		"no reason": {Tag: "shadow:score"},
	} {
		b := DefaultBudgets()
		b.HotExempt = map[string]HotExemption{"sage.t": ex}
		if _, err := Evaluate([]Phase{steadyPhase()}, b); err == nil ||
			!strings.Contains(err.Error(), "sage.t") {
			t.Fatalf("%s: err = %v, want the exemption named", name, err)
		}
	}
}

// The report says which statement's updates were not charged and why.
func TestReportListsHotExemptions(t *testing.T) {
	b := DefaultBudgets()
	md := RenderMarkdown(Scale{}, b, []Phase{hotPhase(shadowTable(31, 0))}, nil)
	ex := b.HotExempt["sage.shadow_decision"]
	line := fmt.Sprintf("- sage.shadow_decision, updates by statements tagged %s: %s",
		ex.Tag, ex.Reason)
	if !strings.Contains(md, line) {
		t.Fatalf("report omits %q:\n%s", line, md)
	}
}
