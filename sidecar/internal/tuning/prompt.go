package tuning

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

// systemPrompt is the tuning agent's instructions. Everything about the
// database arrives in the user message's data block and from the tools.
var systemPrompt = strings.Join([]string{
	"You are pg_sage's PostgreSQL tuning agent. You examine one workload case " +
		"at a time (a top statement, a regression, or write amplification) and " +
		"propose typed changes that fix it, or none.",
	"Use the read-only tools to look before you propose: statement, table, explain, " +
		"whatif_index (HypoPG), write_cost, extended_stats, and rehearse when it is " +
		"offered. Every tool result has an evidence ID (R1, R2, ...).",
	"Rules:",
	"1. Propose only the allowed types listed with the case, as JSON objects; never " +
		"raw SQL. pg_sage generates and validates every statement itself.",
	"2. Every proposal cites the evidence it rests on (S, T, F and R IDs) and " +
		"predicts its effect: expected_change_pct, negative when the metric falls " +
		"(statement time for indexes and hints, temp spills for work_mem, dead tuples " +
		"for autovacuum settings, row-estimate error for statistics). An index drop " +
		"predicts no read change and needs no number.",
	"3. Stay inside the case: its statements and tables only.",
	"4. Confirmed facts bind. Never propose a change a fact forbids; a change to an " +
		"object owned by the application's migrations becomes a migration for the " +
		"application, so say so in the rationale.",
	"5. Do not repeat an index shape listed as already measured and rejected.",
	"6. Do not propose an index an existing or in-flight index already serves, or one " +
		"that would make such an index redundant (same leading keys with a weaker " +
		"predicate or more keys): a replacement is not an independent create.",
	"7. An index costs every insert and non-HOT update; check write_cost on " +
		"write-heavy tables. Drop only indexes that are never used or covered by " +
		"another index, never unique or primary keys.",
	"8. work_mem applies per sort or hash node per connection: keep max_connections " +
		"x work_mem x hash_mem_multiplier within memory, and never above 256MB " +
		"without saying why. Never set an autovacuum scale factor to 0.",
	"9. Hints use pg_hint_plan syntax for one statement, table-scoped (IndexScan, " +
		"BitmapScan, NoSeqScan, HashJoin, ...). Do not use Set(enable_seqscan off) " +
		"or other planner toggles.",
	"10. Extended statistics: 2 to 8 plain columns of one table, kinds ndistinct, " +
		"dependencies or mcv, when correlated columns are misestimated.",
	"11. If nothing is warranted, answer {\"proposals\":[]} with a note.",
	llm.UntrustedDataRule,
}, "\n")

// typeShapes are the JSON forms of the proposal types.
var typeShapes = map[ProposalType]string{
	ProposeIndexCreate: `{"type":"index_create","ddl":"CREATE INDEX CONCURRENTLY <name> ` +
		`ON <schema.table> (...)", "target_queryids":[...]}`,
	ProposeIndexDrop: `{"type":"index_drop","index":"<schema.index>"}`,
	ProposeGUC:       `{"type":"guc","name":"<setting>","value":"<value>"}`,
	ProposeReloption: `{"type":"reloption","table":"<schema.table>","option":"<name>",` +
		`"value":"<v>"}`,
	ProposeStatistics: `{"type":"create_statistics","table":"<schema.table>",` +
		`"columns":[...],"kinds":[...]}`,
	ProposeQueryHint: `{"type":"query_hint","queryid":<id>,"hint":"<pg_hint_plan hint>"}`,
}

// answerInstructions tells the model the answer format and the types it
// may propose for this case.
func answerInstructions(allowed []ProposalType) string {
	var b strings.Builder
	b.WriteString("Allowed proposal types (each also takes rationale, evidence and " +
		"expected_change_pct):\n")
	for _, t := range allowed {
		b.WriteString("- " + string(t) + ": " + typeShapes[t] + "\n")
	}
	if len(allowed) == 0 {
		b.WriteString("(none: answer with an empty list)\n")
	}
	b.WriteString(`Answer with JSON only: {"proposals":[...],"notes":"..."}`)
	return b.String()
}

// allowedTypes are the proposal types this agent may make: switched on and,
// for hints, pg_hint_plan available.
func (a *Agent) allowedTypes() []ProposalType {
	var out []ProposalType
	for _, t := range proposalTypes {
		if a.allowed(t) {
			out = append(out, t)
		}
	}
	return out
}

func (a *Agent) allowed(t ProposalType) bool {
	if !a.settings.Allowed[t] {
		return false
	}
	switch t {
	case ProposeQueryHint:
		return a.deps.Hints != nil && a.deps.Hints.HintsAvailable()
	case ProposeIndexCreate, ProposeIndexDrop:
		return a.deps.Indexes != nil
	}
	return true
}

// clip bounds s to n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}

// oneLine collapses whitespace runs to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
