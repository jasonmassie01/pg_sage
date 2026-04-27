package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/llm"
)

// LLMJsonbAnalyzer enriches JSONB findings with query-level evidence
// from pg_stat_statements via LLM analysis.
type LLMJsonbAnalyzer struct {
	pool      *pgxpool.Pool
	llmClient *llm.Client
	logFn     func(string, string, ...any)
}

// llmJsonbMatch represents one JSONB column the LLM confirmed is
// referenced in a JOIN or WHERE clause.
type llmJsonbMatch struct {
	Schema       string `json:"schema"`
	Table        string `json:"table"`
	Column       string `json:"column"`
	UsedIn       string `json:"used_in"`
	QuerySnippet string `json:"query_snippet"`
}

const jsonbSystemPrompt = `You are a PostgreSQL query analyst. ` +
	`Given a list of JSONB columns and slow queries, identify which ` +
	`JSONB columns are used in JOIN conditions or WHERE clauses.
Return ONLY a JSON array of objects: ` +
	`[{"schema":"...","table":"...","column":"...","used_in":"join|where|both","query_snippet":"..."}]
Return an empty array [] if no JSONB columns are used in joins or where clauses.`

const slowQuerySQL = `
SELECT query, calls, mean_exec_time, rows
  FROM pg_stat_statements
 WHERE query ~* any($1)
 ORDER BY mean_exec_time DESC
 LIMIT 50`

// Enhance enriches JSONB-related findings with LLM analysis.
// If the LLM is unavailable the original findings are returned.
func (a *LLMJsonbAnalyzer) Enhance(
	ctx context.Context, findings []Finding,
) []Finding {
	if a.llmClient == nil || !a.llmClient.IsEnabled() {
		return findings
	}

	jsonbFindings, tables := a.extractJsonbFindings(findings)
	if len(jsonbFindings) == 0 {
		return findings
	}

	matches, err := a.queryAndAnalyze(ctx, jsonbFindings, tables)
	if err != nil {
		a.logFn("warn", "llm jsonb enhancement failed: %v", err)
		return findings
	}

	return a.applyMatches(findings, jsonbFindings, matches)
}

// extractJsonbFindings partitions findings into JSONB-specific ones
// and collects the unique table names they reference.
func (a *LLMJsonbAnalyzer) extractJsonbFindings(
	findings []Finding,
) (map[string]int, []string) {
	jsonbIdx := make(map[string]int)   // "schema.table.column" -> index
	tableSet := make(map[string]bool)  // unique table names

	for i, f := range findings {
		if f.RuleID != "lint_jsonb_in_joins" {
			continue
		}
		key := f.Schema + "." + f.Table + "." + f.Column
		jsonbIdx[key] = i
		tableSet[f.Table] = true
	}

	tables := make([]string, 0, len(tableSet))
	for t := range tableSet {
		tables = append(tables, t)
	}
	return jsonbIdx, tables
}

// queryAndAnalyze fetches slow queries and asks the LLM to identify
// which JSONB columns appear in JOIN/WHERE clauses.
func (a *LLMJsonbAnalyzer) queryAndAnalyze(
	ctx context.Context,
	jsonbIdx map[string]int,
	tables []string,
) ([]llmJsonbMatch, error) {
	queries, err := a.fetchSlowQueries(ctx, tables)
	if err != nil {
		return nil, fmt.Errorf("fetch slow queries: %w", err)
	}
	if len(queries) == 0 {
		return nil, nil
	}

	userPrompt := a.buildUserPrompt(jsonbIdx, queries)

	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	raw, _, err := a.llmClient.Chat(llmCtx, jsonbSystemPrompt, userPrompt, 4096)
	if err != nil {
		return nil, fmt.Errorf("llm chat: %w", err)
	}

	return parseLLMJsonbResponse(raw)
}

// slowQueryRow holds one row from pg_stat_statements.
type slowQueryRow struct {
	Query        string
	Calls        int64
	MeanExecTime float64
	Rows         int64
}

// fetchSlowQueries returns the top slow queries that reference any
// of the given table names.
func (a *LLMJsonbAnalyzer) fetchSlowQueries(
	ctx context.Context, tables []string,
) ([]slowQueryRow, error) {
	patterns := make([]string, len(tables))
	for i, t := range tables {
		patterns[i] = `\m` + t + `\M`
	}

	rows, err := a.pool.Query(ctx, slowQuerySQL, patterns)
	if err != nil {
		return nil, fmt.Errorf("pg_stat_statements query: %w", err)
	}
	defer rows.Close()

	var result []slowQueryRow
	for rows.Next() {
		var r slowQueryRow
		if err := rows.Scan(&r.Query, &r.Calls, &r.MeanExecTime, &r.Rows); err != nil {
			return nil, fmt.Errorf("scan slow query: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// buildUserPrompt constructs the user portion of the LLM prompt.
func (a *LLMJsonbAnalyzer) buildUserPrompt(
	jsonbIdx map[string]int, queries []slowQueryRow,
) string {
	var b strings.Builder
	b.WriteString("JSONB columns found on large tables without GIN indexes:\n")
	for key := range jsonbIdx {
		fmt.Fprintf(&b, "- %s\n", key)
	}

	b.WriteString("\nTop slow queries referencing these tables:\n")
	for _, q := range queries {
		fmt.Fprintf(&b, "\n-- calls=%d mean_time=%.2fms rows=%d\n%s\n",
			q.Calls, q.MeanExecTime, q.Rows, q.Query)
	}
	return b.String()
}

// parseLLMJsonbResponse extracts the JSON array from an LLM response,
// handling markdown-wrapped fences.
func parseLLMJsonbResponse(raw string) ([]llmJsonbMatch, error) {
	cleaned := stripJsonbToJSON(raw)
	var matches []llmJsonbMatch
	if err := json.Unmarshal([]byte(cleaned), &matches); err != nil {
		return nil, fmt.Errorf("parse llm jsonb response: %w", err)
	}
	return matches, nil
}

// stripJsonbToJSON extracts a JSON array from potentially
// markdown-fenced LLM output.
func stripJsonbToJSON(s string) string {
	s = strings.TrimSpace(s)
	first := strings.Index(s, "[")
	last := strings.LastIndex(s, "]")
	if first >= 0 && last > first {
		return s[first : last+1]
	}
	return stripJsonbMarkdownFences(s)
}

// stripJsonbMarkdownFences removes ```json ... ``` wrappers.
func stripJsonbMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// applyMatches upgrades confirmed JSONB findings and removes those
// the LLM says are not used in joins/where.
func (a *LLMJsonbAnalyzer) applyMatches(
	findings []Finding,
	jsonbIdx map[string]int,
	matches []llmJsonbMatch,
) []Finding {
	confirmed := make(map[string]llmJsonbMatch, len(matches))
	for _, m := range matches {
		key := m.Schema + "." + m.Table + "." + m.Column
		confirmed[key] = m
	}

	var result []Finding
	for i, f := range findings {
		if f.RuleID != "lint_jsonb_in_joins" {
			result = append(result, f)
			continue
		}
		key := f.Schema + "." + f.Table + "." + f.Column
		if _, isJsonb := jsonbIdx[key]; !isJsonb {
			result = append(result, f)
			continue
		}
		m, ok := confirmed[key]
		if !ok {
			// LLM says this column is NOT used in joins/where — filter out.
			a.logFn("info",
				"llm: filtered jsonb finding %s (not used in joins/where)",
				key)
			continue
		}
		// Upgrade the description with LLM evidence.
		findings[i].Description = fmt.Sprintf(
			"JSONB column %s used in %s clauses — %s",
			key, m.UsedIn, m.QuerySnippet)
		findings[i].Impact = fmt.Sprintf(
			"LLM analysis confirmed this column is used in %s clauses. "+
				"Missing GIN/GiST index causes sequential scans on "+
				"queries like: %s", m.UsedIn, m.QuerySnippet)
		result = append(result, findings[i])
	}
	return result
}
