package ask

import (
	"encoding/json"
	"sort"
)

// concepts are short, fixed explanations of how pg_sage works, citable as
// doc:<topic>. They describe the product's rules, not this database.
var concepts = map[string]string{
	"ask_sage": "Ask Sage answers questions about one database from evidence it reads " +
		"with read-only tools: findings, executed actions and their verification " +
		"outcomes, the trust ledger, facts, incidents, investigations, the catalog and " +
		"pg_sage's configuration. Every statement must cite that evidence; a statement " +
		"that cites nothing, cites evidence it did not read, or uses a number the " +
		"evidence does not contain is dropped. Ask Sage never executes, approves or " +
		"confirms anything. A person with the operator role (or an MCP token with the " +
		"propose scope) may have it open an investigation or queue one of pg_sage's own " +
		"findings for approval; the policy gate decides, and a person approves on the " +
		"Actions page.",
	"trust_levels": "Earned autonomy is tracked per database, action family and action " +
		"class in the trust ledger. L0 observes only, L1 recommends, L2 hands a ready " +
		"fix to a person for one-click approval, L3 acts on its own within policy and " +
		"verifies afterwards. A pair is promoted only on verified outcomes and a " +
		"person's approval of the promotion; rollbacks, regressions and rejections " +
		"demote it. Irreversible action classes never exceed L1.",
	"verdicts": "Every action pg_sage might take goes through the policy gate, which " +
		"answers execute (allowed on pg_sage's own initiative), queue_approval (a person " +
		"must approve), park (retry later, e.g. outside the maintenance window or " +
		"while another change holds a lease), observe_only, or blocked (never: an " +
		"emergency stop, a replica, a protected object, a binding fact, an unknown " +
		"action).",
	"facts": "Facts are typed statements about a database that pg_sage cannot see in " +
		"the catalog: an object owned by the application's migrations, test-fixture " +
		"schemas, a replication slot's consumer, an append-only table, a maintenance " +
		"or batch window. Detectors and the model propose them with evidence; only a " +
		"person confirms one, and a confirmed fact only narrows or redirects what " +
		"pg_sage does (for example, a change to an app-owned index becomes a " +
		"source-fix packet instead of DDL).",
	"verification": "After an action runs, pg_sage compares the targeted queries " +
		"before and after (call-weighted), records the predicted effect next to the " +
		"observed one and decides a verdict: improved, neutral, regressed, " +
		"insufficient_evidence or unverifiable, with a tolerance of met, partial, " +
		"missed or no_prediction. Regressions roll back where the action is " +
		"reversible, and only verified outcomes earn trust.",
	"investigations": "An investigation diagnoses an incident or an operator's question " +
		"with a causal graph over read-only probes; with an LLM the investigator plans " +
		"its own reads and concludes with cited evidence. Its root cause stays " +
		"advisory unless the family earned root authority on the held-out bench. " +
		"Investigations never change the database; remediation is a separate, gated " +
		"proposal.",
}

// ConceptTopics lists the concept topics, sorted.
func ConceptTopics() []string {
	out := make([]string, 0, len(concepts))
	for k := range concepts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func conceptEnum() string {
	raw, err := json.Marshal(ConceptTopics())
	if err != nil {
		return "[]"
	}
	return string(raw)
}
