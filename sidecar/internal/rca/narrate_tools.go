package rca

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
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
		Description: "Return one evidence item of this incident by id.",
		Parameters: json.RawMessage(`{"type":"object","properties":` +
			`{"id":{"type":"string","description":"evidence id, e.g. E1"}},` +
			`"required":["id"]}`),
	},
}

const narrationSystemPrompt = "You narrate a PostgreSQL incident for an " +
	"on-call engineer. Use only this incident's evidence items; call " +
	"get_evidence to read one. Return ONLY a JSON object: " +
	`{"summary": "<at most 3 sentences>", "evidence_ids": ["E1", ...]}. ` +
	"List every evidence id you relied on. Every number in the summary " +
	"must appear in a cited evidence item. Do not recommend SQL and do " +
	"not speculate beyond the evidence.\n\n" + llm.UntrustedDataRule

// evidenceSet maps evidence ids (E1..En, in causal-chain order) to text.
type evidenceSet struct {
	ids  []string
	text map[string]string
	desc map[string]string
}

func newEvidenceSet(inc Incident) evidenceSet {
	ev := evidenceSet{text: make(map[string]string),
		desc: make(map[string]string)}
	for i, l := range inc.CausalChain {
		id := fmt.Sprintf("E%d", i+1)
		ev.ids = append(ev.ids, id)
		ev.text[id] = linkText(l)
		ev.desc[id] = l.Description
	}
	return ev
}

func (ev evidenceSet) index() string {
	var b strings.Builder
	for _, id := range ev.ids {
		fmt.Fprintf(&b, "%s: %s\n", id, ev.desc[id])
	}
	return b.String()
}

// run executes one validated tool call against the evidence set.
func (ev evidenceSet) run(call llm.ToolCall) string {
	if call.Name == "list_evidence" {
		return llm.UntrustedData("evidence_index", ev.index())
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

// citedCorpus returns the text of the cited items, rejecting uncited
// narratives and unknown ids.
func (ev evidenceSet) citedCorpus(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", errors.New("summary must cite evidence")
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		text, ok := ev.text[id]
		if !ok {
			return "", fmt.Errorf("unknown evidence id %q", id)
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " "), nil
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
