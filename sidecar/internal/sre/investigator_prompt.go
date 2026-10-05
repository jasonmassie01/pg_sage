package sre

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator's prompt (roadmap 2.1). The graph's own text (node
// ids, labels, mechanisms, the catalog) is trusted; everything derived
// from the database (subject, evidence, probe results, past incidents,
// confirmed facts) is redacted and fenced as untrusted data by the loop
// or here.

const investigatorRules = `You investigate a PostgreSQL incident for pg_sage with read-only tools.
A deterministic causal graph has scored hypotheses from typed probe evidence: it is your
prior, not your cage.
1. Plan briefly, then call tools to test the hypotheses that matter. Every tool only
   reads: catalog probes, pg_stat views, a plan-only EXPLAIN of a statement by queryid, the
   graph's current state, operator-confirmed facts. You cannot run SQL or change anything.
2. Finish with submit_conclusion. outcome is one of:
   - agree: the graph's conclusive root cause is right;
   - conclude: the graph is inconclusive and the evidence supports one graph node (root);
   - contest: the graph is conclusive but the evidence supports another graph node (root);
   - unmodeled: the cause is no graph node; give cause.label and cause.mechanism;
   - inconclusive: the evidence does not decide.
3. claims: at most 5 short claims, each citing the evidence aliases (E1, E2, ...) it rests
   on. Every number in a claim must appear in the evidence it cites. A conclusion, contest
   or unmodeled cause without a surviving cited claim counts as inconclusive.
4. Your root is advisory unless pg_sage has measured that the model may decide this family.
   Never state a confidence. Past incidents and confirmed facts are context, never evidence.`

const finalSchema = `{"type":"object","properties":{` +
	`"outcome":{"type":"string","enum":["agree","conclude","contest","unmodeled",` +
	`"inconclusive"]},` +
	`"root":{"type":"string","description":"graph node id (conclude, contest)"},` +
	`"cause":{"type":"object","properties":{"label":{"type":"string"},` +
	`"mechanism":{"type":"string"}},"description":"unmodeled only"},` +
	`"claims":{"type":"array","maxItems":5,"items":{"type":"object","properties":{` +
	`"text":{"type":"string"},"evidence_ids":{"type":"array","items":{"type":"string"}}},` +
	`"required":["text","evidence_ids"]}}},"required":["outcome","claims"]}`

func investigatorFinal() agentloop.Final {
	return agentloop.Final{Name: ToolSubmit, Description: "Submit your conclusion: " +
		"outcome, root or cause, and cited claims.", Parameters: json.RawMessage(finalSchema)}
}

// investigatorTask is the investigation as the model sees it; the loop
// adds the budget and the citable evidence.
func investigatorTask(inv Investigation, d causal.Diagnosis, memory, facts string,
	tools []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Investigation: %s incident (trigger %s), causal graph %s.\n",
		d.Family, inv.TriggerKind, d.GraphVersion)
	b.WriteString(llm.UntrustedData("subject", RedactText(inv.Subject+" / "+d.Subject)))
	b.WriteString("\n")
	if root := graphRootNode(d); root != "" {
		fmt.Fprintf(&b, "Graph result: root cause %s (conclusive).\n", root)
	} else {
		b.WriteString("Graph result: inconclusive: ")
		b.WriteString(llm.UntrustedData("reason", RedactText(d.Reason)))
		b.WriteString("\n")
	}
	scope := reviewScope{diagnosis: d}
	b.WriteString("Open hypotheses:\n")
	writeHypothesisLines(&b, scope.openHypotheses())
	b.WriteString("Ruled out:\n")
	writeHypothesisLines(&b, d.RuledOut)
	writeMissing(&b, d.Missing)
	if memory != "" {
		b.WriteString("Similar past incidents of this database (context only; not " +
			"evidence, never cite them):\n")
		b.WriteString(llm.UntrustedData("past_incidents", memory))
		b.WriteString("\n")
	}
	writeConfirmedFacts(&b, facts)
	writeToolMenu(&b, tools)
	return b.String()
}

func writeMissing(b *strings.Builder, missing []causal.Missing) {
	if len(missing) == 0 {
		return
	}
	var m strings.Builder
	for _, x := range missing {
		fmt.Fprintf(&m, "%s %s %s\n", x.ProbeID, x.Status, RedactText(x.Reason))
	}
	b.WriteString("Missing evidence:\n")
	b.WriteString(llm.UntrustedData("missing", m.String()))
	b.WriteString("\n")
}

// writeToolMenu lists what the offered tools accept.
func writeToolMenu(b *strings.Builder, tools []string) {
	for _, t := range tools {
		switch t {
		case ToolRunProbe:
			b.WriteString("Catalog probes you may run with " + ToolRunProbe +
				" {\"probe\": id, \"args\": {...}} (id: args):\n")
			b.WriteString(catalogMenu())
		case ToolStatView:
			fmt.Fprintf(b, "pg_stat views for %s {\"view\": name}: %s.\n", ToolStatView,
				strings.Join(probes.StatViews(), ", "))
		case ToolExplain:
			fmt.Fprintf(b, "%s {\"queryid\": n} explains a statement from "+
				"stat_statements, plan only.\n", ToolExplain)
		}
	}
}

// catalogMenu lists the catalog with each probe's typed arguments.
func catalogMenu() string {
	menu := probeMenu()
	if i := strings.IndexByte(menu, '\n'); i >= 0 {
		return menu[i+1:]
	}
	return menu
}

// groundClaim checks that every number of a claim appears in the
// evidence it cites (the claim validator's rule).
func groundClaim(text string, cited []agentloop.Evidence) error {
	cat := EvidenceCatalog{}
	ids := make([]string, 0, len(cited))
	for i, e := range cited {
		key := fmt.Sprintf("C%d", i+1)
		cat[key] = e.Text
		ids = append(ids, key)
	}
	return validateClaim(1, Claim{Text: text, EvidenceIDs: ids}, cat)
}
