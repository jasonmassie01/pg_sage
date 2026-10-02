package runbook

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The compiler's prompt. The catalogs (probes and their columns, graph
// nodes, trigger kinds, action types) are trusted; the playbook is
// untrusted data, fenced and never part of the system prompt.

const compileRules = `You compile an English PostgreSQL incident playbook into a typed pg_sage
runbook: a DAG of probe steps, decision nodes and proposals. A human reviews and signs the
draft before it can ever run; nothing you write is executed.
Rules:
1. Use only the trigger kinds, catalog probes, columns, causal graph nodes and action types
   listed in the user message. Never write SQL, shell commands or free-form actions. A step
   the catalogs cannot express becomes an "escalate" proposal.
2. Nodes: {"id","type":"probe","probe","args"?,"next"} |
   {"id","type":"decision","when","then","else"} |
   {"id","type":"proposal","proposal":{"kind","node"?,"action_type"?}}.
   ids are lower_snake_case. The DAG is acyclic, every node is reachable from "start" and
   every path ends in a proposal; at most 32 nodes and 8 probe steps. "args" is only
   {"window_seconds": n} for probes that take a window.
3. Predicates ("when"), deterministic over the latest result of a probe run in an earlier
   step, or over the causal graph's diagnosis:
   {"op":"probe_status","probe","in":[ok|empty|error|no_privilege|unsupported]}
   {"op":"row_count","probe","cmp","value"}
   {"op":"column","probe","column","agg":max|min|sum|any|all,"cmp","value"}
   {"op":"column_text","probe","column","text"}
   {"op":"hypothesis","node","in":[root_cause|contributing|unproven|ruled_out]}
   {"op":"all"|"any","of":[predicates]} and {"op":"not","of":[predicate]}.
   cmp is one of > >= < <= == != and value is a number.
4. "trigger": {"kinds":[trigger kinds],"nodes":[graph nodes]}; with nodes, the runbook only
   runs when one of them is an open hypothesis of the investigation.
5. Proposals: "operator_step" names the graph node whose manual step applies; "action"
   names an action type and the graph node it addresses; "escalate" hands over to a DBA.
`

const definitionSchema = `{"type":"object","additionalProperties":false,"properties":{` +
	`"name":{"type":"string","maxLength":120},` +
	`"description":{"type":"string","maxLength":1000},` +
	`"trigger":{"type":"object","additionalProperties":false,"properties":{` +
	`"kinds":{"type":"array","items":{"type":"string"}},` +
	`"nodes":{"type":"array","items":{"type":"string"}}},"required":["kinds"]},` +
	`"start":{"type":"string"},` +
	`"nodes":{"type":"array","maxItems":32,"items":{"type":"object",` +
	`"additionalProperties":false,"properties":{"id":{"type":"string"},` +
	`"type":{"type":"string","enum":["probe","decision","proposal"]},` +
	`"note":{"type":"string","maxLength":200},"probe":{"type":"string"},` +
	`"args":{"type":"object","additionalProperties":false,` +
	`"properties":{"window_seconds":{"type":"integer"}}},` +
	`"next":{"type":"string"},` +
	`"when":{"type":"object","description":"a predicate (see the rules)"},` +
	`"then":{"type":"string"},"else":{"type":"string"},` +
	`"proposal":{"type":"object","additionalProperties":false,"properties":{` +
	`"kind":{"type":"string","enum":["operator_step","action","escalate"]},` +
	`"node":{"type":"string"},"action_type":{"type":"string"}},"required":["kind"]}},` +
	`"required":["id","type"]}}},"required":["name","trigger","start","nodes"]}`

func compileTools() []llm.ToolSpec {
	return []llm.ToolSpec{{Name: compileToolName,
		Description: "Submit the compiled runbook definition (a draft for human review).",
		Parameters:  json.RawMessage(definitionSchema)}}
}

func compileMessages(req CompileRequest, repair string, tools bool) []llm.Message {
	system := compileRules + llm.UntrustedDataRule + "\n"
	if tools {
		system += "Call " + compileToolName + " exactly once with the runbook."
	} else {
		system += "Respond with only one JSON object, no prose, matching this JSON " +
			"schema:\n" + definitionSchema
	}
	user := vocabularyMenu(req.Vocab) + "Playbook to compile:\n" +
		llm.UntrustedData("playbook", req.Text) + "\n"
	msgs := []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	if repair != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: "Your previous reply was " +
			"rejected (" + repair + "). Reply again with one corrected runbook that " +
			"follows every rule, through " + compileToolName + " or as the JSON object."})
	}
	return msgs
}

// vocabularyMenu lists everything a definition may reference.
func vocabularyMenu(v Vocab) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trigger kinds: %s\n", strings.Join(v.TriggerKinds, ", "))
	b.WriteString("Catalog probes (id [args]: columns):\n")
	maxWindow := int64(probes.MaxWindow / time.Second)
	for _, id := range probes.Catalog().IDs() {
		spec, _ := probes.Catalog().Spec(id)
		args := ""
		switch spec.Args {
		case probes.ArgsBackend:
			continue // backend probes need evidence-bound identities
		case probes.ArgsWindow:
			args = fmt.Sprintf(" [window_seconds 60-%d]", maxWindow)
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", id, args, strings.Join(OutputColumns(spec), ", "))
	}
	b.WriteString("Causal graph nodes (id (family): label):\n")
	for _, n := range causal.Graph() {
		fmt.Fprintf(&b, "- %s (%s): %s\n", n.ID, n.Family, n.Label)
	}
	fmt.Fprintf(&b, "Action types for \"action\" proposals: %s\n",
		strings.Join(ActionTypes(), ", "))
	return b.String()
}
