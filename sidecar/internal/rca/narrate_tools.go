package rca

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The narration tool catalog is read-only and in-memory: the model can
// only read evidence the engine already holds for this incident. It
// never reaches the database, and tool results are fenced as untrusted
// data.

var narrationTools = []llm.ToolSpec{
	{
		Name:        "list_evidence",
		Description: "List the evidence items of this incident.",
	},
	{
		Name:        "get_evidence",
		Description: "Return one evidence item of this incident by id (E#, P# or H#).",
		Parameters: json.RawMessage(`{"type":"object","properties":` +
			`{"id":{"type":"string","description":"evidence id, e.g. E1 or P2"}},` +
			`"required":["id"]}`),
	},
	{
		Name: "get_probe_result",
		Description: "Return the result of a catalog probe already collected " +
			"for this incident, e.g. lock_graph.",
		Parameters: json.RawMessage(`{"type":"object","properties":` +
			`{"probe":{"type":"string","description":"catalog probe id"}},` +
			`"required":["probe"]}`),
	},
}

const narrationSystemPrompt = "You narrate a PostgreSQL incident for an " +
	"on-call engineer. Use only this incident's evidence: its own items " +
	"(E#), catalog probe results (P#) and deterministic hypotheses (H#). " +
	"Read them with get_evidence or get_probe_result. Return ONLY a JSON " +
	`object: {"claims": [{"text": "<one sentence>", "evidence_ids": ` +
	`["E1", ...]}]} with at most 3 claims. Every claim must cite the ` +
	"evidence it rests on, and every number in a claim must appear in the " +
	"evidence that claim cites. Do not recommend SQL and do not speculate " +
	"beyond the evidence.\n\n" + llm.UntrustedDataRule

// evidenceSet maps evidence ids to text: the incident's causal chain
// (E1..En), catalog probe results (P1..Pn) and hypotheses (H1..Hn).
type evidenceSet struct {
	ids    []string
	text   map[string]string
	desc   map[string]string
	probes map[string]string // probe id -> evidence id
}

func newEvidenceSet(inc Incident, pe incidentEvidence) evidenceSet {
	ev := evidenceSet{text: make(map[string]string),
		desc: make(map[string]string), probes: make(map[string]string)}
	for i, l := range inc.CausalChain {
		ev.add(fmt.Sprintf("E%d", i+1), l.Description, linkText(l))
	}
	for _, o := range pe.observations {
		text := o.Result.Text(probeTextRows)
		ev.add(o.EvidenceID, strings.SplitN(text, "\n", 2)[0], text)
		ev.probes[string(o.Result.ProbeID)] = o.EvidenceID
	}
	for i, h := range pe.hypotheses {
		ev.add(fmt.Sprintf("H%d", i+1), fmt.Sprintf("%s (%s)", h.Label, h.Status),
			hypothesisText(h))
	}
	return ev
}

func (ev *evidenceSet) add(id, desc, text string) {
	ev.ids = append(ev.ids, id)
	ev.desc[id] = desc
	ev.text[id] = text
}

func (ev evidenceSet) index() string {
	var b strings.Builder
	for _, id := range ev.ids {
		fmt.Fprintf(&b, "%s: %s\n", id, ev.desc[id])
	}
	return b.String()
}

// catalog is the evidence in scope for the claim validator.
func (ev evidenceSet) catalog() sre.EvidenceCatalog {
	out := make(sre.EvidenceCatalog, len(ev.text))
	for id, text := range ev.text {
		out[id] = text
	}
	return out
}

// run executes one validated tool call against the evidence set.
func (ev evidenceSet) run(call llm.ToolCall) string {
	switch call.Name {
	case "list_evidence":
		return llm.UntrustedData("evidence_index", ev.index())
	case "get_probe_result":
		return ev.probeResult(call.Arguments)
	}
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return fmt.Sprintf("invalid get_evidence arguments: %v", err)
	}
	text, ok := ev.text[args.ID]
	if !ok {
		return fmt.Sprintf("unknown evidence id %q; valid ids: %s",
			args.ID, strings.Join(ev.ids, ", "))
	}
	return llm.UntrustedData("evidence_"+args.ID, text)
}

func (ev evidenceSet) probeResult(raw json.RawMessage) string {
	var args struct {
		Probe string `json:"probe"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("invalid get_probe_result arguments: %v", err)
	}
	id, ok := ev.probes[args.Probe]
	if !ok {
		collected := make([]string, 0, len(ev.probes))
		for p := range ev.probes {
			collected = append(collected, p)
		}
		sort.Strings(collected)
		return fmt.Sprintf("probe %q was not collected for this incident; "+
			"collected: %s", args.Probe, strings.Join(collected, ", "))
	}
	return llm.UntrustedData("evidence_"+id, id+": "+ev.text[id])
}

func narrationPrompt(inc Incident, ev evidenceSet) []llm.Message {
	user := fmt.Sprintf("Incident severity: %s. Signals: %s.\n"+
		"Deterministic root cause:\n%s\nEvidence items:\n%s",
		inc.Severity, strings.Join(inc.SignalIDs, ", "),
		llm.UntrustedData("root_cause", inc.RootCause),
		llm.UntrustedData("evidence_index", ev.index()))
	return []llm.Message{
		{Role: "system", Content: narrationSystemPrompt},
		{Role: "user", Content: user},
	}
}
