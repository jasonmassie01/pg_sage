package perfgate

import (
	"fmt"
	"strings"
	"time"
)

const topStatements = 15

// RenderMarkdown is the gate's report: verdict, scale, budgets, ranked
// offenders, unconfirmed generic-plan suspects, statements that could not
// be planned, the costliest statements, table activity and endpoints.
func RenderMarkdown(s Scale, b Budgets, phases []Phase, offenders []Offender) string {
	var sb strings.Builder
	verdict := "PASS"
	if len(offenders) > 0 {
		verdict = "FAIL"
	}
	fmt.Fprintf(&sb, "# pg_sage performance gate: %s\n\n", verdict)
	fmt.Fprintf(&sb, "Scale %s: %d tables, %d indexes, %d sequences in %d schemas "+
		"(%d identical clones); %d history rows per growing sage table.\n\n",
		s.Name, s.Tables(), s.Indexes(), s.Sequences(), s.Schemas, s.CloneSchemas,
		s.HistoryRows)
	for _, p := range phases {
		fmt.Fprintf(&sb, "- phase %s: %s, %d cycles, %d statements, %d plans\n", p.Name,
			p.Window.Round(time.Second), p.Cycles, len(p.Statements), len(p.Plans))
	}
	writeBudgets(&sb, b)
	writeOffenderTable(&sb, offenders)
	writeSuspects(&sb, phases, b)
	writeUnexplainable(&sb, phases)
	writeTopStatements(&sb, phases)
	writeTables(&sb, phases)
	writeEndpoints(&sb, phases)
	return sb.String()
}

func writeBudgets(sb *strings.Builder, b Budgets) {
	sb.WriteString("\n## Budgets\n\n| Gate | Budget |\n|---|---|\n")
	fmt.Fprintf(sb, "| %s | no seq scan of a sage table above %d rows |\n",
		GateSeqScan, b.SeqScanMinRows)
	fmt.Fprintf(sb, "| %s | %.0f ms (steady phase) |\n", GateStatementMean, b.StatementMeanMs)
	fmt.Fprintf(sb, "| %s | %.0f ms (steady phase) |\n", GateCycleDBTime, b.CycleDBTimeMs)
	fmt.Fprintf(sb, "| %s | %d per sage table (steady phase) |\n", GateRowsWritten,
		b.RowsWrittenPerCycle)
	fmt.Fprintf(sb, "| %s | %.0f ms |\n", GateCatalogMax, b.CatalogStatementMaxMs)
	fmt.Fprintf(sb, "| %s | none |\n", GateTimeout)
	fmt.Fprintf(sb, "| %s | HTTP 200 within %.0f ms |\n", GateEndpoint, b.EndpointMaxMs)
}

func writeOffenderTable(sb *strings.Builder, offenders []Offender) {
	fmt.Fprintf(sb, "\n## Offenders (%d)\n\n", len(offenders))
	if len(offenders) == 0 {
		sb.WriteString("None.\n")
		return
	}
	sb.WriteString("| # | Gate | Phase | Subject | Measured | Budget | Detail |\n")
	sb.WriteString("|---|---|---|---|---|---|---|\n")
	for i, o := range offenders {
		fmt.Fprintf(sb, "| %d | %s | %s | %s | %.0f %s | %.0f | %s |\n", i+1, o.Gate,
			o.Phase, cell(o.Subject), o.Measured, o.Unit, o.Budget, cell(o.Detail))
	}
}

// writeSuspects lists generic plans that scan a large sage table which the
// statistics did not see scanned: with real parameters the plan differed.
func writeSuspects(sb *strings.Builder, phases []Phase, b Budgets) {
	var lines []string
	for _, p := range phases {
		large, scanned := map[string]bool{}, map[string]bool{}
		for _, t := range p.Tables {
			large[t.Name] = t.LiveRows > b.SeqScanMinRows
			scanned[t.Name] = t.SeqScans > 0
		}
		for _, plan := range p.Plans {
			for _, s := range plan.SeqScans {
				name := s.Schema + "." + s.Relation
				if large[name] && !scanned[name] {
					lines = append(lines, fmt.Sprintf("- %s: %s in queryid %d: %s", p.Name,
						name, plan.Statement.QueryID, shortQuery(plan.Statement.Query)))
				}
			}
		}
	}
	fmt.Fprintf(sb, "\n## Generic-plan suspects (%d)\n\n", len(lines))
	sb.WriteString("Generic plans that scan a large sage table the statistics did not " +
		"see scanned (the custom plan used an index). Not offenders.\n\n")
	sb.WriteString(strings.Join(lines, "\n") + "\n")
}

func writeUnexplainable(sb *strings.Builder, phases []Phase) {
	var lines []string
	for _, p := range phases {
		for _, plan := range p.Plans {
			if plan.Err != "" {
				lines = append(lines, fmt.Sprintf("- %s queryid %d: %s (%s)", p.Name,
					plan.Statement.QueryID, shortQuery(plan.Statement.Query), plan.Err))
			}
		}
	}
	fmt.Fprintf(sb, "\n## Unexplainable statements (%d)\n\n", len(lines))
	sb.WriteString(strings.Join(lines, "\n") + "\n")
}

func writeTopStatements(sb *strings.Builder, phases []Phase) {
	for _, p := range phases {
		fmt.Fprintf(sb, "\n## Costliest statements, %s\n\n", p.Name)
		sb.WriteString("| queryid | calls | total ms | mean ms | max ms | rows | statement |\n")
		sb.WriteString("|---|---|---|---|---|---|---|\n")
		for i, s := range p.Statements {
			if i == topStatements {
				break
			}
			fmt.Fprintf(sb, "| %d | %d | %.1f | %.2f | %.1f | %d | %s |\n", s.QueryID,
				s.Calls, s.TotalMs, s.MeanMs, s.MaxMs, s.Rows, cell(shortQuery(s.Query)))
		}
	}
}

func writeTables(sb *strings.Builder, phases []Phase) {
	sb.WriteString("\n## Sage table activity\n\n")
	sb.WriteString("| phase | table | live rows | seq scans | seq rows read | idx scans " +
		"| rows written |\n|---|---|---|---|---|---|---|\n")
	for _, p := range phases {
		for _, t := range p.Tables {
			if t.SeqScans == 0 && t.RowsWritten == 0 && t.IdxScans == 0 {
				continue
			}
			fmt.Fprintf(sb, "| %s | %s | %d | %d | %d | %d | %d |\n", p.Name, t.Name,
				t.LiveRows, t.SeqScans, t.SeqTupRead, t.IdxScans, t.RowsWritten)
		}
	}
}

func writeEndpoints(sb *strings.Builder, phases []Phase) {
	sb.WriteString("\n## API list endpoints\n\n| endpoint | status | ms |\n|---|---|---|\n")
	for _, p := range phases {
		for _, e := range p.Endpoints {
			fmt.Fprintf(sb, "| %s | %d | %.1f |\n", e.Path, e.Status,
				float64(e.Duration)/float64(time.Millisecond))
		}
	}
}

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
