package sre

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// docs/sage-sre-permissions-and-data-flow.md is the page security reviewers
// read. It must describe the code, so the numbers and ids it states are
// checked against the code here (sre-10). No concurrency, state or
// integration tests: the contract only reads one file and package values.

const sreDocPath = "../../../docs/sage-sre-permissions-and-data-flow.md"

// staleDocClaims are statements the code has outgrown: M5 ships an
// approval-gated pg_cancel_backend, M6 maintenance statistics, and the
// investigator (the default mode) makes more than two model calls.
var staleDocClaims = []string{
	"R1 has no action",
	"R1 never signals a backend",
	"at most 2 model calls",
	"no `pg_signal_backend`",
}

func readSREDoc(path string) (string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("reading the Sage SRE permissions doc %s: %w", path, err)
	}
	// A Windows checkout (core.autocrlf) has CRLF line ends; the heading
	// and row checks split on "\n".
	return strings.ReplaceAll(string(raw), "\r\n", "\n"), nil
}

// docProblems is every way doc contradicts the code; none means it holds.
func docProblems(doc string) []string {
	var out []string
	for _, claim := range staleDocClaims {
		if strings.Contains(doc, claim) {
			out = append(out, fmt.Sprintf("stale claim %q", claim))
		}
	}
	out = append(out, planProblems(doc)...)
	out = append(out, limitProblems(doc)...)
	out = append(out, privilegeProblems(doc)...)
	return out
}

// thousands renders n the way the doc writes numbers: 32,000.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// tableRow is the cells of the first table row whose first cell is
// "`key`", or nil.
func tableRow(doc, key string) []string {
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cells[0] == "`"+key+"`" {
			return cells
		}
	}
	return nil
}

// planProblems checks the investigator plan table: | plan | used for |
// steps | probes | wall | tokens | step cap | tools |.
func planProblems(doc string) []string {
	var out []string
	for name, p := range DefaultInvestigatorPlans() {
		row := tableRow(doc, name)
		if len(row) < 8 {
			out = append(out, fmt.Sprintf("plan %q has no 8-column row", name))
			continue
		}
		want := []string{strconv.Itoa(p.MaxSteps), strconv.Itoa(p.MaxProbes),
			fmt.Sprintf("%d s", int(p.Wall/time.Second)), thousands(p.MaxTokens),
			thousands(int64(p.StepTokens))}
		for i, w := range want {
			if row[i+2] != w {
				out = append(out, fmt.Sprintf("plan %q column %d is %q, want %q", name,
					i+3, row[i+2], w))
			}
		}
		out = append(out, planToolProblems(name, p, row[7])...)
	}
	for kind, name := range triggerPlans {
		if row := tableRow(doc, name); len(row) < 2 ||
			!strings.Contains(row[1], "`"+string(kind)+"`") {
			out = append(out, fmt.Sprintf("plan %q row does not name trigger %q", name, kind))
		}
	}
	return out
}

func planToolProblems(name string, p InvestigatorPlan, cell string) []string {
	var out []string
	for _, tool := range p.Tools {
		if !strings.Contains(cell, "`"+tool+"`") {
			out = append(out, fmt.Sprintf("plan %q does not list tool %q", name, tool))
		}
	}
	for kind, tools := range p.TriggerTools {
		for _, tool := range tools {
			if !strings.Contains(cell, "`"+tool+"`") ||
				!strings.Contains(cell, "`"+string(kind)+"`") {
				out = append(out, fmt.Sprintf("plan %q does not list %q for %q", name,
					tool, kind))
			}
		}
	}
	return out
}

// limitPhrases are the doc's statements of the code's ceilings.
func limitPhrases() []string {
	l := DefaultLimits()
	return []string{
		fmt.Sprintf("no plan can exceed %s tokens", thousands(CeilingInvestigatorTokens)),
		fmt.Sprintf("%s reasoning tokens per call", thousands(maxInvestigatorReasoning)),
		fmt.Sprintf("capped at %d rows and %s characters", maxResultRows,
			thousands(maxResultRunes)),
		fmt.Sprintf("at most %d model turns, %s input and %s output tokens, plus %s "+
			"reasoning tokens", l.MaxModelTurns, thousands(l.MaxInputTokens),
			thousands(l.MaxOutputTokens), thousands(l.MaxReasoningTokens)),
		fmt.Sprintf("a %d ms statement timeout", probes.MaxStatementTimeout.Milliseconds()),
		fmt.Sprintf("at most %d rows and %d KiB per probe", probes.MaxRows,
			probes.MaxBytes>>10),
		fmt.Sprintf("one probe at a time per database and %d per sidecar",
			probes.MaxSidecarConcurrency),
		fmt.Sprintf("at most %d probes in %d s of active time", l.MaxProbes,
			int(l.MaxActive/time.Second)),
		fmt.Sprintf("has a statement timeout of up to %d s",
			int(probes.MaxBackgroundStatementTimeout/time.Second)),
	}
}

func limitProblems(doc string) []string {
	var out []string
	flat := strings.Join(strings.Fields(doc), " ")
	for _, phrase := range limitPhrases() {
		if !strings.Contains(flat, phrase) {
			out = append(out, fmt.Sprintf("missing limit statement %q", phrase))
		}
	}
	return out
}

// section is doc from the heading line to the next heading of any level
// at or above it, or "".
func section(doc, heading string) string {
	start := strings.Index(doc, "\n"+heading+"\n")
	if start < 0 {
		return ""
	}
	rest := doc[start+len(heading)+2:]
	level := strings.Repeat("#", strings.Count(strings.Fields(heading)[0], "#"))
	for i := 1; i <= len(level); i++ {
		marker := "\n" + strings.Repeat("#", i) + " "
		if end := strings.Index(rest, marker); end >= 0 {
			rest = rest[:end]
		}
	}
	return rest
}

// readIDs are every read an investigation can make: the catalog, the
// plan-only EXPLAIN, the historical reads and the detection snapshot.
func readIDs() []string {
	var out []string
	for _, id := range probes.Catalog().IDs() {
		out = append(out, string(id))
	}
	return append(out, string(ExplainProbeID), ToolHistory, string(IncidentSnapshotProbeID))
}

// actionIDs are the approved actions and the reads made around them.
func actionIDs() []string {
	out := []string{"cancel_backend", "create_statistics"}
	for _, id := range probes.ActionRegistry().IDs() {
		out = append(out, string(id))
	}
	return out
}

func privilegeProblems(doc string) []string {
	var out []string
	checks := []struct {
		heading string
		ids     []string
	}{{"### Investigation privileges", readIDs()}, {"### Action privileges", actionIDs()}}
	for _, c := range checks {
		sec := section(doc, c.heading)
		if sec == "" {
			out = append(out, fmt.Sprintf("no %q section", c.heading))
			continue
		}
		listed := firstColumnIDs(sec)
		for _, id := range c.ids {
			if !listed[id] {
				out = append(out, fmt.Sprintf("%q has no row for %q", c.heading, id))
			}
		}
	}
	history := section(doc, "### Investigation privileges")
	for _, id := range probes.HistoricalIDs() {
		if !strings.Contains(history, "`"+string(id)+"`") {
			out = append(out, fmt.Sprintf("read_history source %q is not named", id))
		}
	}
	if !strings.Contains(section(doc, "### Action privileges"), "`pg_signal_backend`") {
		out = append(out, "the action privileges do not name pg_signal_backend")
	}
	return out
}

// firstColumnIDs are the backticked names in the first cell of each table
// row of sec ("| `a`, `b` | ...").
func firstColumnIDs(sec string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(sec, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cell := strings.SplitN(strings.Trim(line, "|"), "|", 2)[0]
		parts := strings.Split(cell, "`")
		for i := 1; i < len(parts); i += 2 {
			out[parts[i]] = true
		}
	}
	return out
}

func TestDocsContract_DocMatchesTheCode(t *testing.T) {
	doc, err := readSREDoc(sreDocPath)
	if err != nil {
		t.Fatal(err)
	}
	if problems := docProblems(doc); len(problems) > 0 {
		t.Fatalf("docs/sage-sre-permissions-and-data-flow.md contradicts the code:\n%s",
			strings.Join(problems, "\n"))
	}
}
