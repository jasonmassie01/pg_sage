package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// openGate names the objects that already carry an open advisor finding.
// They are left out of this cycle's prompts and any new finding about one
// is dropped, so the rest of a category is still evaluated (advisors-02):
// one open finding no longer silences its whole sub-advisor.
type openGate struct {
	idents   map[string]bool            // category|object_identifier
	gucs     map[string]map[string]bool // category -> setting name
	queryIDs map[int64]bool
	tables   map[string]bool // canonicalTable
}

func newOpenGate(open []analyzer.Finding) openGate {
	g := openGate{idents: map[string]bool{}, gucs: map[string]map[string]bool{},
		queryIDs: map[int64]bool{}, tables: map[string]bool{}}
	for _, f := range open {
		g.idents[f.Category+"|"+f.ObjectIdentifier] = true
		switch f.Category {
		case "wal_tuning", "connection_tuning":
			if guc := findingGUC(f); guc != "" {
				if g.gucs[f.Category] == nil {
					g.gucs[f.Category] = map[string]bool{}
				}
				g.gucs[f.Category][guc] = true
			}
		case "query_rewrite":
			if qid, ok := rewriteQueryID(f.ObjectIdentifier); ok {
				g.queryIDs[qid] = true
			}
		case "bloat_remediation":
			g.tables[canonicalTable(f.ObjectIdentifier)] = true
		}
	}
	return g
}

func (g openGate) hasGUC(category, name string) bool {
	return g.gucs[category][strings.ToLower(name)]
}

// openGUCs lists a category's gated settings, sorted, for the prompt.
func (g openGate) openGUCs(category string) []string {
	var out []string
	for name := range g.gucs[category] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (g openGate) hasTable(t collector.TableStats) bool {
	return g.tables[canonicalTable(t.SchemaName+"."+t.RelName)]
}

// dropOpen removes the new findings about an already-open object: the
// deterministic backstop behind the prompt filter, since a model may
// still propose a setting it was not shown.
func (g openGate) dropOpen(category string, findings []analyzer.Finding,
	logFn func(string, string, ...any)) []analyzer.Finding {
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		f = g.rekey(category, f)
		if g.covers(category, f) {
			logFn("DEBUG", "advisor: %s: dropping new finding for %s, an open finding "+
				"covers it", category, f.ObjectIdentifier)
			continue
		}
		out = append(out, f)
	}
	return out
}

// rekey gives a setting finding whose key an open finding about another
// setting already holds the per-setting key split statements use
// ("instance:<guc>"). A single-statement proposal is keyed "instance",
// like the open row, so dropping it by key would silence the rest of the
// category again, and keeping its key would overwrite the open row.
func (g openGate) rekey(category string, f analyzer.Finding) analyzer.Finding {
	if !configCategories[category] || !g.idents[f.Category+"|"+f.ObjectIdentifier] {
		return f
	}
	guc := findingGUC(f)
	if guc == "" || g.hasGUC(category, guc) || strings.HasSuffix(f.ObjectIdentifier, ":"+guc) {
		return f
	}
	f.ObjectIdentifier += ":" + guc
	f.Title = fmt.Sprintf("%s: set %s", category, guc)
	return f
}

func (g openGate) covers(category string, f analyzer.Finding) bool {
	if g.idents[f.Category+"|"+f.ObjectIdentifier] {
		return true
	}
	switch category {
	case "wal_tuning", "connection_tuning":
		return g.hasGUC(category, findingGUC(f))
	case "query_rewrite":
		qid, ok := rewriteQueryID(f.ObjectIdentifier)
		return ok && g.queryIDs[qid]
	case "bloat_remediation":
		return g.tables[canonicalTable(f.ObjectIdentifier)]
	}
	return false
}

// canonicalTable folds a table reference to lower-case schema.table,
// public when unqualified, without identifier quotes.
func canonicalTable(ref string) string {
	s := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(ref), `"`, ""))
	if s != "" && !strings.Contains(s, ".") {
		s = "public." + s
	}
	return s
}

// gucNote tells the model which settings were left out of its context.
func gucNote(g openGate, category string) string {
	names := g.openGUCs(category)
	if len(names) == 0 {
		return ""
	}
	return "\n\nSettings left out (an open recommendation already covers them; do " +
		"not recommend them): " + strings.Join(names, ", ")
}

type openGateKey struct{}

func withOpenGate(ctx context.Context, g openGate) context.Context {
	return context.WithValue(ctx, openGateKey{}, g)
}

// openGateFrom is the cycle's gate; outside a cycle nothing is gated.
func openGateFrom(ctx context.Context) openGate {
	g, _ := ctx.Value(openGateKey{}).(openGate)
	return g
}

// qualifiesForBloat is the bloat advisor's floor: at least 1000 rows and
// at least 10% of them dead.
func qualifiesForBloat(t collector.TableStats) bool {
	total := t.NLiveTup + t.NDeadTup
	if total < 1000 {
		return false
	}
	return float64(t.NDeadTup)/float64(max(total, 1)) >= 0.10
}
