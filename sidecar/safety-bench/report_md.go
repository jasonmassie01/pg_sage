package safetybench

import (
	"fmt"
	"sort"
	"strings"
)

// Markdown renders the report as the human-readable summary published with
// the release.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# AgentSafetyBench v0\n\n")
	fmt.Fprintf(&b, "- Generated: %s\n", r.GeneratedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "- Server: %s\n", oneLine(r.ServerVersion))
	if r.PgSageVersion != "" || r.PgSageCommit != "" {
		fmt.Fprintf(&b, "- pg_sage: %s %s\n", r.PgSageVersion, r.PgSageCommit)
	}
	if !r.SQLParser {
		b.WriteString("- SQL parser: not in this build (CGO_ENABLED=0); explain_guard and " +
			"agent_query refuse every statement, so their rows are not authoritative\n")
	}
	b.WriteString("\n")
	r.writeReadOnly(&b)
	r.writeExpectMismatches(&b)
	r.writePosture(&b)
	r.writeIncidents(&b)
	return b.String()
}

func (r Report) writeReadOnly(b *strings.Builder) {
	b.WriteString("## Read-only designs\n\n")
	b.WriteString("Which read-only designs refuse which cases, checksums intact. " +
		"Not measured (n/m) means the statement failed for a reason no design is " +
		"credited with (other_error); it never counts as held. A leaked_in_error is " +
		"NOT HELD: the statement wrote nothing, but its error carried a value the case " +
		"protects.\n\n")
	b.WriteString("| Design | Held | Not measured | Total |\n|---|---|---|---|\n")
	sum := r.ReadOnlySummary()
	designs := append([]string(nil), r.Designs...)
	for _, d := range designs {
		s := sum[d]
		fmt.Fprintf(b, "| %s | %d | %d | %d |\n", d, s.Held, s.NotMeasured, s.Total)
	}
	b.WriteString("\n| Case | Technique | ")
	b.WriteString(strings.Join(designs, " | "))
	b.WriteString(" |\n|---|---|")
	for range designs {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, c := range r.ReadOnly {
		fmt.Fprintf(b, "| %s | %s |", c.ID, oneLine(c.Technique))
		byDesign := map[string]Attempt{}
		for _, a := range c.Attempts {
			byDesign[a.Design] = a
		}
		for _, d := range designs {
			fmt.Fprintf(b, " %s |", cell(byDesign[d]))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	r.writeLeaks(b)
}

// writeLeaks adds a "Leaked in error" note for every leaked_in_error cell.
// The value itself is never printed (the attempt detail redacts it).
func (r Report) writeLeaks(b *strings.Builder) {
	var notes []string
	for _, c := range r.ReadOnly {
		for _, a := range c.Attempts {
			if a.Observed == ClassLeakedInError {
				notes = append(notes, fmt.Sprintf("- %s under %s: the error carried a "+
					"value the case declares as a sentinel; it reached the caller.\n",
					c.ID, a.Design))
			}
		}
	}
	if len(notes) == 0 {
		return
	}
	b.WriteString("**Leaked in error**\n\n")
	b.WriteString(strings.Join(notes, ""))
	b.WriteString("\n")
}

// cell renders one attempt: held, n/m (not measured) or NOT HELD, with the
// observed class.
func cell(a Attempt) string {
	mark := "**NOT HELD**"
	switch {
	case a.Held():
		mark = "held"
	case a.NotMeasured():
		mark = "n/m"
	}
	return fmt.Sprintf("%s (%s)", mark, a.Observed)
}

// writeExpectMismatches lists the cases whose canonical design observed a
// class other than the declared one.
func (r Report) writeExpectMismatches(b *strings.Builder) {
	fmt.Fprintf(b, "## Declared vs observed (%s)\n\n", canonicalDesign)
	mm := r.ExpectMismatches()
	if len(mm) == 0 {
		b.WriteString("Every case observed its declared class.\n\n")
		return
	}
	b.WriteString("A credited refusal of another class fails the bench; an execution " +
		"or an other_error is scored in the table above.\n\n")
	b.WriteString("| Case | Declared | Observed | Outcome |\n|---|---|---|---|\n")
	for _, m := range mm {
		outcome := "scored above"
		if m.Fails() {
			outcome = "fails the bench"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", m.ID, m.Expect, m.Observed, outcome)
	}
	b.WriteString("\n")
}

func (r Report) writePosture(b *strings.Builder) {
	b.WriteString("## Posture scenarios\n\n")
	if !r.PostureWired {
		b.WriteString("Detector framework not connected: scenarios set up and recorded, " +
			"no detections claimed. Wire a PostureProvider to score them.\n\n")
	}
	b.WriteString("| Scenario | Expect | Matched | Missing | Note |\n|---|---|---|---|---|\n")
	for _, p := range r.Posture {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", p.Name,
			strings.Join(p.Expect, ", "), strings.Join(p.MatchedDetectors, ", "),
			strings.Join(p.MissingDetectors, ", "), oneLine(p.VersionNote))
	}
	b.WriteString("\n")
}

func (r Report) writeIncidents(b *strings.Builder) {
	b.WriteString("## Incident-to-control mapping\n\n")
	fmt.Fprintf(b, "%d rows: %d exercised in v0, %d declared, not measured, "+
		"%d out of scope, %d future release.\n\n",
		r.IncidentScore.Total, r.IncidentScore.Exercised, r.IncidentScore.Declared,
		r.IncidentScore.OutOfScope, r.IncidentScore.Future)
	b.WriteString("| Incident | Expected | v0 status | Detectors | Why |\n")
	b.WriteString("|---|---|---|---|---|\n")
	rows := append([]IncidentExpectation(nil), r.Incidents...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, e := range rows {
		fmt.Fprintf(b, "| %s %s | %s | %s | %s | %s |\n", e.ID, oneLine(e.Title),
			e.Expected, e.StatusIn(r.Posture), strings.Join(e.Detectors, ", "),
			oneLine(e.Why))
	}
	b.WriteString("\n")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	return strings.TrimSpace(s)
}
