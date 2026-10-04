package sre

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The model turn's prompt (AI-SRE-SPEC §2.2/§2.9). The graph's own text
// (node ids, labels, mechanisms) is trusted; everything derived from the
// database (subject, facts, probe reasons) is redacted and fenced as
// untrusted data. The model gets exactly one tool, submit_review, or the
// same schema as JSON-schema prompting when the provider cannot call
// tools.

const reviewSchema = schemaHead + schemaProbe + schemaClaims

// reviewSchemaNoProbe is the tool schema of a turn that may not ask for a
// probe.
const reviewSchemaNoProbe = schemaHead + schemaClaims

const schemaHead = `{"type":"object","additionalProperties":false,` +
	`"properties":{` +
	`"ranking":{"type":"array","items":{"type":"string"},` +
	`"description":"every open hypothesis node id, most likely first"},`

const schemaProbe = `"next_probe":{"type":"object","additionalProperties":false,` +
	`"description":"only when probes are offered: one catalog probe",` +
	`"properties":{"probe":{"type":"string"},` +
	`"args":{"type":"object","additionalProperties":false,"properties":{` +
	`"pid":{"type":"integer"},"backend_start":{"type":"string"},` +
	`"window_seconds":{"type":"integer"}}},` +
	`"rationale":{"type":"string","maxLength":300}},` +
	`"required":["probe","args","rationale"]},`

const schemaClaims = `"claims":{"type":"array","maxItems":5,"items":{"type":"object",` +
	`"additionalProperties":false,"properties":{"text":{"type":"string"},` +
	`"evidence_ids":{"type":"array","items":{"type":"string"}}},` +
	`"required":["text","evidence_ids"]}}},` +
	`"required":["ranking","claims"]}`

const reviewRules = `You review a PostgreSQL incident investigation for pg_sage.
A deterministic causal graph has already scored the hypotheses from typed probe evidence.
The graph is the authority. You may only:
1. ranking: order ALL open hypotheses (by node id) from most to least likely. You cannot
   add, remove or rename hypotheses or change their status. If you believe the evidence
   contradicts the graph's root cause, rank your choice first: the graph's root cause
   stays authoritative and your disagreement is recorded for an operator.
2. next_probe: only when catalog probes are offered, ask for ONE of them with its typed
   args and a one-line rationale naming the hypotheses its result tells apart. Otherwise
   omit next_probe. Never write SQL.
3. claims: up to 5 short claims about what the evidence shows. Each claim cites the
   evidence ids (E1, E2, ...) it rests on. Every number in a claim must appear in the
   evidence it cites; never compute or estimate new numbers.
4. Similar past incidents (P1, P2, ...), when shown, are context only. They are not
   evidence of this incident: never cite them and never take numbers from them.
`

// reviewTools is the one tool of a turn that may ask for a probe.
func reviewTools() []llm.ToolSpec { return reviewToolsFor(true) }

// reviewToolsFor drops next_probe from the schema when no probe is offered.
func reviewToolsFor(allowProbe bool) []llm.ToolSpec {
	schema := reviewSchemaNoProbe
	if allowProbe {
		schema = reviewSchema
	}
	return []llm.ToolSpec{{Name: reviewToolName,
		Description: "Submit your review of the investigation: ranking, optional " +
			"next_probe and cited claims.",
		Parameters: json.RawMessage(schema)}}
}

// reviewMessages builds one turn: the rules, the investigation and, for
// a repair turn, why the previous reply was rejected.
func reviewMessages(inv Investigation, s reviewScope, repair string,
	tools bool) []llm.Message {
	system := reviewRules + llm.UntrustedDataRule + "\n"
	if tools {
		system += "Call " + reviewToolName + " exactly once with your review."
	} else {
		system += "Respond with only one JSON object, no prose, matching this JSON " +
			"schema:\n" + reviewSchema
	}
	msgs := []llm.Message{{Role: "system", Content: system},
		{Role: "user", Content: s.userPrompt(inv)}}
	if repair != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: "Your previous reply " +
			"was rejected (" + truncateRunes(repair, 300) + "). Reply again with one " +
			"corrected review that follows every rule."})
	}
	return msgs
}

func (s reviewScope) userPrompt(inv Investigation) string {
	d := s.diagnosis
	var b strings.Builder
	fmt.Fprintf(&b, "Investigation: %s incident (trigger %s), causal graph %s.\n",
		d.Family, inv.TriggerKind, d.GraphVersion)
	b.WriteString(llm.UntrustedData("subject", RedactText(inv.Subject+" / "+d.Subject)))
	b.WriteString("\n")
	if d.Conclusive && d.Root != nil {
		fmt.Fprintf(&b, "Graph result: root cause %s (conclusive).\n", d.Root.Node)
	} else {
		b.WriteString("Graph result: inconclusive.\n")
	}
	b.WriteString("Open hypotheses (rank all of these):\n")
	writeHypothesisLines(&b, s.openHypotheses())
	b.WriteString("Ruled out (do not rank):\n")
	writeHypothesisLines(&b, d.RuledOut)
	b.WriteString("Evidence (cite by id):\n")
	b.WriteString(llm.UntrustedData("evidence", s.evidenceBlock()))
	b.WriteString("\n")
	if s.memory != "" {
		b.WriteString("Similar past incidents of this database (context only; they are " +
			"not evidence and cannot be cited):\n")
		b.WriteString(llm.UntrustedData("past_incidents", s.memory))
		b.WriteString("\n")
	}
	writeConfirmedFacts(&b, s.facts)
	if s.allowProbe {
		b.WriteString(probeMenu())
	} else {
		b.WriteString("No probes are offered: omit next_probe.\n")
	}
	return b.String()
}

func (s reviewScope) openHypotheses() []causal.Hypothesis {
	var out []causal.Hypothesis
	if s.diagnosis.Root != nil {
		out = append(out, *s.diagnosis.Root)
	}
	return append(append(out, s.diagnosis.Contributing...), s.diagnosis.Alternatives...)
}

func writeHypothesisLines(b *strings.Builder, hs []causal.Hypothesis) {
	if len(hs) == 0 {
		b.WriteString("- none\n")
	}
	for _, h := range hs {
		fmt.Fprintf(b, "- %s [%s, graph score %.2f]: %s. %s\n", h.Node, h.Status,
			h.Confidence, h.Label, h.Mechanism)
	}
}

func (s reviewScope) evidenceBlock() string {
	var b strings.Builder
	for _, e := range s.evidence {
		state := ""
		if e.stale {
			state = " (stale: hash mismatch, do not cite)"
		}
		fmt.Fprintf(&b, "%s [%s %s]%s %s\n", e.alias, e.probe, e.status, state,
			strings.TrimPrefix(e.text, e.probe+" "+e.status))
	}
	for _, m := range s.diagnosis.Missing {
		fmt.Fprintf(&b, "missing: %s %s %s\n", m.ProbeID, m.Status,
			RedactText(m.Reason))
	}
	return b.String()
}

// probeMenu lists the catalog with each probe's typed arguments.
func probeMenu() string {
	var b strings.Builder
	b.WriteString("Catalog probes you may propose as next_probe (id: args):\n")
	for _, id := range probes.Catalog().IDs() {
		spec, _ := probes.Catalog().Spec(id)
		switch spec.Args {
		case probes.ArgsBackend:
			fmt.Fprintf(&b, "- %s: {\"pid\": integer, \"backend_start\": RFC 3339 "+
				"timestamp}\n", id)
		case probes.ArgsWindow:
			fmt.Fprintf(&b, "- %s: {} or {\"window_seconds\": 60-%d}\n", id,
				maxWindowSeconds)
		default:
			fmt.Fprintf(&b, "- %s: {}\n", id)
		}
	}
	return b.String()
}
