package tuner

import (
	"fmt"
	"strings"
)

const maxTunerPromptChars = 14000
const maxPlanJSONChars = 4000

// TunerSystemPrompt returns the system prompt for LLM hint reasoning.
func TunerSystemPrompt() string {
	return `CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning, no explanation, no markdown fences, no text before or after the array. Start your response with [ and end with ].

You are a PostgreSQL query tuning expert specializing in pg_hint_plan directives. Given a slow query with detected performance symptoms, its execution plan, table statistics, indexes, and system context, prescribe the optimal pg_hint_plan hints.

Rules:
1. Output ONLY valid pg_hint_plan syntax: Set(), HashJoin(), MergeJoin(), NestLoop(), IndexScan(), IndexOnlyScan(), SeqScan(), NoSeqScan(), Parallel(), NoParallel().
2. Join strategy: choose based on table sizes, join selectivity, and whether join columns have high correlation (high correlation + sorted data favors MergeJoin). Small outer with indexed inner favors NestLoop. Large unsorted joins favor HashJoin. Do NOT blindly default to HashJoin.
3. IndexScan: ONLY prescribe if a suitable index EXISTS in the provided index list. If no matching index exists, omit the hint and explain in rationale.
4. work_mem: calculate from spill size, but divide by active_backends to avoid memory contention. Never exceed shared_buffers / max_connections.
5. Parallel workers: scale to table size (1M+ rows). Never exceed the system max_parallel_workers_per_gather value.
6. Sort+Limit: if a sort processes 10x+ more rows than LIMIT needs, prescribe IndexScan on sort columns IF a suitable index exists.
7. Consider ALL symptoms together and produce ONE coherent hint string that addresses the root cause.
8. If deterministic fallback hints are provided, improve on them or confirm them if correct.
9. Confidence: 0.0-1.0 based on completeness of available data and certainty of recommendation.
10. Query rewrite: if the hint is a workaround and the query itself could be rewritten for better performance (e.g., replacing correlated subqueries with JOINs, using EXISTS instead of IN, removing unnecessary DISTINCT, adding proper WHERE clauses), include the rewritten SQL in "suggested_rewrite" and explain why in "rewrite_rationale". Only suggest a rewrite when it would meaningfully improve performance beyond what hints alone achieve. Leave both fields empty string if no rewrite is needed.

Output format:
[{"hint_directive": "Set(work_mem \"256MB\") HashJoin(t1 t2)", "rationale": "Why these hints help", "confidence": 0.85, "suggested_rewrite": "", "rewrite_rationale": ""}]

If no hints are warranted, return: []`
}

// FormatTunerPrompt builds the user prompt from a QueryContext.
func FormatTunerPrompt(qctx QueryContext) string {
	var b strings.Builder
	c := qctx.Candidate

	fmt.Fprintf(&b, "## Query (queryid: %d)\n", c.QueryID)
	fmt.Fprintf(&b, "%s\n\n", c.Query)
	fmt.Fprintf(&b,
		"Mean exec: %.1fms | Calls: %d | "+
			"Temp blks written: %d | Mean plan: %.1fms\n\n",
		c.MeanExecTime, c.Calls,
		c.TempBlksWritten, c.MeanPlanTime,
	)

	formatSymptoms(&b, qctx.Symptoms)
	formatPlan(&b, qctx.PlanJSON)
	formatTables(&b, qctx.Tables)
	formatSystem(&b, qctx.System)
	formatFallback(&b, qctx.FallbackHints)

	prompt := b.String()
	if len(prompt) > maxTunerPromptChars {
		return truncatePrompt(qctx)
	}
	b.WriteString(
		"\nRESPOND NOW with ONLY the JSON array. " +
			"Start with [ immediately.",
	)
	return b.String()
}

func formatSymptoms(b *strings.Builder, symptoms []PlanSymptom) {
	b.WriteString("## Detected Symptoms\n")
	for _, s := range symptoms {
		line := fmt.Sprintf("- %s", s.Kind)
		if s.RelationName != "" {
			line += fmt.Sprintf(" on %s", s.RelationName)
		}
		for k, v := range s.Detail {
			line += fmt.Sprintf(", %s=%v", k, v)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

func formatPlan(b *strings.Builder, planJSON string) {
	if planJSON == "" {
		return
	}
	b.WriteString("## Execution Plan\n")
	if len(planJSON) > maxPlanJSONChars {
		b.WriteString(planJSON[:maxPlanJSONChars])
		b.WriteString("\n... (truncated)\n")
	} else {
		b.WriteString(planJSON)
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func formatTables(b *strings.Builder, tables []TableDetail) {
	for _, t := range tables {
		fmt.Fprintf(b, "## Table: %s.%s\n", t.Schema, t.Name)
		fmt.Fprintf(b, "Live tuples: %d | Dead tuples: %d",
			t.LiveTuples, t.DeadTuples)
		if t.TableBytes > 0 {
			fmt.Fprintf(b, " | Size: %s", humanBytes(t.TableBytes))
		}
		b.WriteString("\n")

		if len(t.Columns) > 0 {
			b.WriteString("### Columns\n")
			for _, c := range t.Columns {
				nullable := ""
				if c.IsNullable {
					nullable = " (nullable)"
				}
				fmt.Fprintf(b, "- %s %s%s\n",
					c.Name, c.Type, nullable)
			}
		}

		if len(t.Indexes) > 0 {
			b.WriteString("### Indexes\n")
			for _, idx := range t.Indexes {
				unique := ""
				if idx.IsUnique {
					unique = " [UNIQUE]"
				}
				fmt.Fprintf(b, "- %s%s: %s (scans: %d)\n",
					idx.Name, unique, idx.Definition, idx.Scans)
			}
		}

		if len(t.ColStats) > 0 {
			b.WriteString("### Column Stats\n")
			for _, s := range t.ColStats {
				fmt.Fprintf(b,
					"- %s: n_distinct=%.2f, correlation=%.4f\n",
					s.Column, s.NDistinct, s.Correlation)
			}
		}
		b.WriteString("\n")
	}
}

func formatSystem(b *strings.Builder, sys SystemContext) {
	b.WriteString("## System\n")
	fmt.Fprintf(b,
		"active_backends=%d, max_connections=%d, "+
			"work_mem=%s, shared_buffers=%s, "+
			"effective_cache_size=%s, "+
			"max_parallel_workers_per_gather=%d\n\n",
		sys.ActiveBackends, sys.MaxConnections,
		sys.WorkMem, sys.SharedBuffers,
		sys.EffCacheSize, sys.MaxParallelPG,
	)
}

func formatFallback(b *strings.Builder, fallbackHints string) {
	if fallbackHints == "" {
		return
	}
	b.WriteString("## Deterministic Fallback Hints\n")
	b.WriteString(fallbackHints)
	b.WriteString("\n\n")
}

// truncatePrompt rebuilds the prompt with reduced context.
func truncatePrompt(qctx QueryContext) string {
	var b strings.Builder
	c := qctx.Candidate

	fmt.Fprintf(&b, "## Query (queryid: %d)\n", c.QueryID)
	fmt.Fprintf(&b, "%s\n\n", c.Query)
	fmt.Fprintf(&b,
		"Mean exec: %.1fms | Calls: %d | "+
			"Temp blks written: %d\n\n",
		c.MeanExecTime, c.Calls, c.TempBlksWritten,
	)

	formatSymptoms(&b, qctx.Symptoms)

	// Truncated plan
	if qctx.PlanJSON != "" {
		b.WriteString("## Execution Plan (truncated)\n")
		planLimit := 2000
		if len(qctx.PlanJSON) > planLimit {
			b.WriteString(qctx.PlanJSON[:planLimit])
		} else {
			b.WriteString(qctx.PlanJSON)
		}
		b.WriteString("\n\n")
	}

	// First table only, no col stats
	if len(qctx.Tables) > 0 {
		t := qctx.Tables[0]
		fmt.Fprintf(&b, "## Table: %s.%s\n", t.Schema, t.Name)
		fmt.Fprintf(&b, "Live tuples: %d\n", t.LiveTuples)
		if len(t.Indexes) > 0 {
			b.WriteString("### Indexes\n")
			for _, idx := range t.Indexes {
				fmt.Fprintf(&b, "- %s: %s (scans: %d)\n",
					idx.Name, idx.Definition, idx.Scans)
			}
		}
		b.WriteString("\n")
	}

	formatSystem(&b, qctx.System)
	formatFallback(&b, qctx.FallbackHints)

	b.WriteString(
		"\nRESPOND NOW with ONLY the JSON array. " +
			"Start with [ immediately.",
	)
	return b.String()
}

func humanBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
