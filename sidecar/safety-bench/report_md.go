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
	b.WriteString("\n")
	r.writeReadOnly(&b)
	r.writePosture(&b)
	r.writeIncidents(&b)
	return b.String()
}

func (r Report) writeReadOnly(b *strings.Builder) {
	b.WriteString("## Read-only designs\n\n")
	b.WriteString("Which read-only designs refuse which cases, checksums intact.\n\n")
	b.WriteString("| Design | Held | Total |\n|---|---|---|\n")
	sum := r.ReadOnlySummary()
	designs := append([]string(nil), r.Designs...)
	for _, d := range designs {
		s := sum[d]
		fmt.Fprintf(b, "| %s | %d | %d |\n", d, s.Held, s.Total)
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
}

// cell renders one attempt: held or NOT, with the observed class.
func cell(a Attempt) string {
	mark := "held"
	if !a.Held() {
		mark = "**NOT HELD**"
	}
	return fmt.Sprintf("%s (%s)", mark, a.Observed)
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
	fmt.Fprintf(b, "%d rows: %d exercised in v0, %d out of scope, %d future release.\n\n",
		r.IncidentScore.Total, r.IncidentScore.Exercised, r.IncidentScore.OutOfScope,
		r.IncidentScore.Future)
	b.WriteString("| Incident | Expected | v0 status | Detectors | Why |\n")
	b.WriteString("|---|---|---|---|---|\n")
	rows := append([]IncidentExpectation(nil), r.Incidents...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, e := range rows {
		fmt.Fprintf(b, "| %s %s | %s | %s | %s | %s |\n", e.ID, oneLine(e.Title),
			e.Expected, e.Status(), strings.Join(e.Detectors, ", "), oneLine(e.Why))
	}
	b.WriteString("\n")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	return strings.TrimSpace(s)
}
