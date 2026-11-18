package tuning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The read-only tools the model may call while it examines a case. Each
// successful result is registered as citable evidence (R1, R2, ...); a
// refusal is an error result and registers nothing. Results are bounded
// and wrapped as untrusted data. No tool writes to the monitored
// database: explain never runs ANALYZE, whatif_index uses hypothetical
// indexes, rehearse builds on a disposable clone.

const (
	maxToolResultBytes = 8000
	maxPlanChars       = 6000
	maxWhatIfPerCase   = 3
	// maxHintExplainPerCase bounds hinted_explain calls per case (each plans
	// the statement twice).
	maxHintExplainPerCase = 3
)

// cycleTools is tool state shared by the cases of one cycle: whether the
// cycle's rehearsal was used, and its tier-2 ladder record by index shape.
type cycleTools struct {
	mu         sync.Mutex
	rehearsed  bool
	rehearsals map[string]verify.LadderRecord
}

func (a *Agent) cycleTools() *cycleTools { return &cycleTools{} }

// rehearsalKey is an index's shape (table and fingerprint), so the record
// of a rehearsed DDL finds the proposal of the same index.
func rehearsalKey(ddl string) string {
	spec, err := optimizer.ParseIndexDDL(ddl)
	if err != nil {
		return ""
	}
	return strings.ToLower(spec.TableSchema+"."+spec.TableName) + "|" + spec.Fingerprint()
}

func (c *cycleTools) recordRehearsal(ddl string, rec verify.LadderRecord) {
	key := rehearsalKey(ddl)
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rehearsals == nil {
		c.rehearsals = map[string]verify.LadderRecord{}
	}
	c.rehearsals[key] = rec
}

// rehearsalOf is the tier-2 record of the index ddl creates, if the cycle
// rehearsed it.
func (c *cycleTools) rehearsalOf(ddl string) (verify.LadderRecord, bool) {
	key := rehearsalKey(ddl)
	if c == nil || key == "" {
		return verify.LadderRecord{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.rehearsals[key]
	return rec, ok
}

// toolbox runs the tools for one case.
type toolbox struct {
	a         *Agent
	c         Case
	cur, prev *collector.Snapshot
	w         Workload
	cycle     *cycleTools
	evidence  evidenceSet
	next      int
	whatIfs   int
	hinted    int
	seen      map[string]string
}

func (a *Agent) newToolbox(c Case, cur, prev *collector.Snapshot, w Workload,
	cycle *cycleTools) *toolbox {
	return &toolbox{a: a, c: c, cur: cur, prev: prev, w: w, cycle: cycle,
		evidence: evidenceSet{}, seen: map[string]string{}}
}

// toolResult is a tool's answer before it is registered and rendered.
type toolResult struct {
	fields map[string]any
	ref    string
	text   string
	err    error
	// plain results (nothing found) are answers but not evidence.
	plain bool
}

func refused(format string, args ...any) toolResult {
	return toolResult{err: fmt.Errorf(format, args...)}
}

// exec runs one tool call and returns its bounded, wrapped result. A
// statement read again returns the same result under the same evidence
// ID (the agent pre-fetches the case's lead statement).
func (tb *toolbox) exec(ctx context.Context, call llm.ToolCall) string {
	var args toolArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return render(map[string]any{"error": "malformed arguments: " + err.Error()})
		}
	}
	key, _ := json.Marshal(args)
	cacheKey := ""
	if call.Name == "statement" {
		cacheKey = call.Name + "|" + string(key)
	}
	if cached, ok := tb.seen[cacheKey]; ok && cacheKey != "" {
		return cached
	}
	res := tb.run(ctx, call.Name, args)
	out := res.fields
	if res.err != nil {
		return render(map[string]any{"error": res.err.Error()})
	}
	if !res.plain {
		tb.next++
		id := fmt.Sprintf("R%d", tb.next)
		out["evidence_id"] = id
		tb.evidence[id] = Evidence{ID: id, Kind: "tool:" + call.Name, Ref: res.ref,
			Text: res.text}
	}
	rendered := render(out)
	if cacheKey != "" {
		tb.seen[cacheKey] = rendered
	}
	return rendered
}

func render(out map[string]any) string {
	raw, err := json.Marshal(out)
	if err != nil {
		raw = []byte(`{"error":"the result could not be encoded"}`)
	}
	return llm.UntrustedData("tool_result", clip(string(raw), maxToolResultBytes))
}

func (tb *toolbox) run(ctx context.Context, name string, args toolArgs) toolResult {
	switch name {
	case "statement":
		return tb.statement(args)
	case "table":
		return tb.table(ctx, args)
	case "explain":
		return tb.explain(ctx, args)
	case "whatif_index":
		return tb.whatIf(ctx, args)
	case "write_cost":
		return tb.writeCost(ctx, args)
	case "extended_stats":
		return tb.extendedStats(ctx, args)
	case "rehearse":
		return tb.rehearse(ctx, args)
	case "hinted_explain":
		return tb.hintedExplain(ctx, args)
	}
	return refused("unknown tool %q", name)
}

// toolArgs are the arguments of every tool.
type toolArgs struct {
	QueryID QueryID  `json:"queryid"`
	Table   string   `json:"table"`
	DDL     string   `json:"ddl"`
	Columns []string `json:"columns"`
	Hint    string   `json:"hint"`
}

func spec(name, description, params string) llm.ToolSpec {
	return llm.ToolSpec{Name: name, Description: description,
		Parameters: json.RawMessage(params)}
}

const (
	queryIDParam = `{"type":"object","properties":{"queryid":{"type":"string",` +
		`"description":"pg_stat_statements queryid"}},"required":["queryid"]}`
	tableParam = `{"type":"object","properties":{"table":{"type":"string",` +
		`"description":"schema.table"}},"required":["table"]}`
	ddlParam = `{"type":"object","properties":{"ddl":{"type":"string",` +
		`"description":"one CREATE INDEX CONCURRENTLY statement"}},"required":["ddl"]}`
	columnsParam = `{"type":"object","properties":{"table":{"type":"string"},` +
		`"columns":{"type":"array","items":{"type":"string"}}},` +
		`"required":["table","columns"]}`
	hintParam = `{"type":"object","properties":{"queryid":{"type":"string",` +
		`"description":"pg_stat_statements queryid of a case statement"},` +
		`"hint":{"type":"string","description":"pg_hint_plan directives, no comment ` +
		`markers"}},"required":["queryid","hint"]}`
)

// ToolNames are the names of the tools the agent offers the model.
func (a *Agent) ToolNames() []string {
	specs := a.toolSpecs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// toolSpecs are the tools offered to the model; rehearse only with a
// clone provider.
func (a *Agent) toolSpecs() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		spec("statement", "Interval and cumulative statistics of a workload statement.",
			queryIDParam),
		spec("table", "A case table: columns, indexes, sizes, write rates, storage "+
			"parameters and workload hints.", tableParam),
		spec("explain", "The statement's plan (cached, or EXPLAIN without ANALYZE).",
			queryIDParam),
		spec("whatif_index", "Measure a CREATE INDEX on a case table with HypoPG "+
			"hypothetical indexes.", ddlParam),
		spec("write_cost", "The write cost of an index on these columns of a case table.",
			columnsParam),
		spec("extended_stats", "Existing extended statistics and the planner statistics "+
			"of these columns.", columnsParam),
	}
	if _, ok := a.hintExplainer(); ok {
		specs = append(specs, spec("hinted_explain", "Plan a case statement plain and "+
			"with a pg_hint_plan hint (EXPLAIN, never ANALYZE) and compare the plans "+
			"and estimated costs.", hintParam))
	}
	if a.deps.Rehearse != nil {
		specs = append(specs, spec("rehearse", "Build the index on a disposable clone "+
			"and plan the case statements before and after (once per cycle).", ddlParam))
	}
	return specs
}
