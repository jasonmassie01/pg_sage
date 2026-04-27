package optimizer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

const maxPromptChars = 16384 // ~4096 tokens at 4 chars/token

// SystemPrompt returns the v2 system prompt for the index optimizer LLM.
func SystemPrompt() string {
	return `CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning, no explanation, no markdown fences, no text before or after the array. Start your response with [ and end with ].

You are a PostgreSQL index optimization expert. Analyze table
structure, query patterns, execution plans, and pg_stats data to recommend
indexes that will improve query performance.

Rules:
1. Always use CREATE INDEX CONCURRENTLY to avoid table locks.
2. Prefer partial indexes when a WHERE clause filters a minority of rows.
3. Use INCLUDE columns for covering indexes only when all query columns fit.
4. Consider GIN for JSONB/array columns, GiST for geometric/text search.
5. Never recommend an index that already exists (same columns, same type).
6. For write-heavy tables (>70% writes), only recommend if improvement >30%.
7. Consider column correlation from pg_stats for B-tree range scan benefit.
8. Respect collation for text indexes with non-C collation.
9. Keep INCLUDE column count <= 3 to avoid index bloat.
10. Maximum 10 indexes per table unless strongly justified.
11. If the plan shows Sort: external merge Disk or Hash Batches > 1, recommend work_mem tuning, NOT an index.
12. If a query scans all rows for aggregation (GROUP BY on full table), recommend a materialized view, NOT an index.
13. Composite index column order matters: a B-tree on (a, b) only helps queries that filter on "a" or "a AND b", NOT queries that filter only on "b". If an existing composite index has the filtered column in a non-leading position, recommend a new single-column index on that column or a reordered composite index. Do not assume an index on (a, b) covers WHERE b = ?.

Output ONLY valid JSON. No markdown fences, no commentary outside JSON.

Output format — a JSON array of objects:
[
  {
    "table": "schema.table",
    "ddl": "CREATE INDEX CONCURRENTLY ...",
    "drop_ddl": "DROP INDEX CONCURRENTLY IF EXISTS ...",
    "rationale": "Why this index helps",
    "severity": "info|warning|critical",
    "index_type": "btree|gin|gist|brin|hash",
    "category": "missing_index|covering_index|partial_index|composite_index",
    "affected_queries": ["query text or ID"],
    "estimated_improvement_pct": 25.0
  }
]

If no indexes are recommended, return an empty array: []`
}

// FormatPrompt builds the user prompt from a TableContext.
func FormatPrompt(tc TableContext) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Table: %s.%s\n", tc.Schema, tc.Table)
	fmt.Fprintf(&b, "Live tuples: %d | Dead tuples: %d\n",
		tc.LiveTuples, tc.DeadTuples)
	fmt.Fprintf(&b, "Table size: %s | Index size: %s\n",
		humanBytes(tc.TableBytes), humanBytes(tc.IndexBytes))
	fmt.Fprintf(&b, "Write rate: %.1f%% | Workload: %s\n",
		tc.WriteRate, tc.Workload)
	collationStr := tc.Collation
	if tc.Collation != "" && tc.Collation != "C" && tc.Collation != "POSIX" {
		collationStr = tc.Collation + " (non-C — LIKE prefix needs pattern_ops)"
	}
	fmt.Fprintf(&b, "Existing indexes: %d | Collation: %s\n",
		tc.IndexCount, collationStr)
	if tc.Relpersistence == "u" {
		b.WriteString("** UNLOGGED TABLE — crash-unsafe, no WAL generated. " +
			"Indexes are also unlogged and lost on crash. " +
			"Optimize for speed, not durability. **\n")
	}
	b.WriteString("\n")

	b.WriteString("### Columns\n")
	for _, c := range tc.Columns {
		nullable := ""
		if c.IsNullable {
			nullable = " (nullable)"
		}
		fmt.Fprintf(&b, "- %s %s%s\n", c.Name, c.Type, nullable)
	}

	if len(tc.ColStats) > 0 {
		b.WriteString("\n### Column Statistics (pg_stats)\n")
		for _, s := range tc.ColStats {
			line := fmt.Sprintf("- %s: n_distinct=%.2f, correlation=%.4f",
				s.Column, s.NDistinct, s.Correlation)
			if len(s.MostCommonVals) > 0 && len(s.MostCommonVals) <= 5 {
				line += fmt.Sprintf(", top_vals=%v", s.MostCommonVals)
			}
			b.WriteString(line + "\n")
		}
	}

	if len(tc.Indexes) > 0 {
		b.WriteString("\n### Existing Indexes\n")
		for _, idx := range tc.Indexes {
			unique := ""
			if idx.IsUnique {
				unique = " [UNIQUE]"
			}
			fmt.Fprintf(&b, "- %s%s: %s (scans: %d)\n",
				idx.Name, unique, idx.Definition, idx.Scans)
		}
	}

	b.WriteString("\n### Queries\n")
	for _, q := range tc.Queries {
		sanitized := llm.SanitizeForLLM(q.Text)
		fmt.Fprintf(&b,
			"- [calls=%d, mean=%.2fms, total=%.2fms] %s\n",
			q.Calls, q.MeanTimeMs, q.TotalTimeMs, sanitized)
	}

	if len(tc.Plans) > 0 {
		b.WriteString("\n### Execution Plans\n")
		for _, p := range tc.Plans {
			fmt.Fprintf(&b, "- QueryID %d: %s\n", p.QueryID, p.Summary)
		}
	}

	// Detection hints for LLM
	if len(tc.ColStats) > 0 {
		// Check for BRIN candidates
		var brinCols []string
		for _, s := range tc.ColStats {
			if s.Correlation > 0.8 || s.Correlation < -0.8 {
				brinCols = append(brinCols, s.Column)
			}
		}
		if len(brinCols) > 0 {
			fmt.Fprintf(&b, "\n### BRIN Candidates (correlation > 0.8)\n")
			for _, col := range brinCols {
				fmt.Fprintf(&b, "- %s\n", col)
			}
		}
	}

	if len(tc.JoinPairs) > 0 {
		b.WriteString("\n### Join Patterns\n")
		for _, jp := range tc.JoinPairs {
			fmt.Fprintf(&b, "- %s ↔ %s: %s (%d queries)\n",
				jp.Left, jp.Right, jp.Condition, len(jp.Queries))
		}
	}

	// Safety valve: if prompt is too large, rebuild with fewer queries.
	prompt := b.String()
	if len(prompt) > maxPromptChars && len(tc.Queries) > 3 {
		return FormatPromptTruncated(tc)
	}

	b.WriteString("\nRESPOND NOW with ONLY the JSON array. Start with [ immediately.")

	return b.String()
}

// FormatPromptTruncated rebuilds the prompt with only the top 3 queries by calls.
func FormatPromptTruncated(tc TableContext) string {
	// Sort queries by calls descending, keep top 3.
	sorted := make([]QueryInfo, len(tc.Queries))
	copy(sorted, tc.Queries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Calls > sorted[j].Calls
	})
	if len(sorted) > 3 {
		sorted = sorted[:3]
	}
	truncated := tc
	truncated.Queries = sorted
	// Also truncate plans to match retained queries.
	queryIDs := make(map[int64]bool)
	for _, q := range sorted {
		queryIDs[q.QueryID] = true
	}
	var plans []PlanSummary
	for _, p := range tc.Plans {
		if queryIDs[p.QueryID] {
			plans = append(plans, p)
		}
	}
	truncated.Plans = plans
	truncated.JoinPairs = nil // drop join pairs to save space
	// Re-format (won't recurse because fewer queries now).
	return FormatPrompt(truncated)
}

// parseRecommendations extracts Recommendation structs from LLM response.
func parseRecommendations(response string) ([]Recommendation, error) {
	var recs []Recommendation
	if err := llm.ParseJSON(response, llm.JSONArray, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// stripToJSON extracts the JSON array from an LLM response that
// may contain thinking text, markdown fences, or other non-JSON
// content. Delegates to the canonical llm.StripJSON.
func stripToJSON(s string) string {
	return llm.StripJSON(s, llm.JSONArray)
}

func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
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
