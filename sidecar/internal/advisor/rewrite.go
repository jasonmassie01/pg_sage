package advisor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const rewriteSystemPrompt = `You are a PostgreSQL query optimization expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. Suggest concrete SQL rewrites with BEFORE and AFTER.
2. Common patterns: correlated subquery->JOIN, IN->EXISTS, ` +
	`SELECT *->columns, N+1->batch, OFFSET->keyset.
3. For each suggestion, explain performance impact.
4. If query is already optimal, return [].
5. Never suggest changes that alter query semantics.
6. If ORM-generated, note app code changes needed.
7. Rate impact: high (>10x), medium (2-10x), low (<2x).
8. Max 3 suggestions per query.
9. These are ALWAYS advisory -- never auto-executed.

Each element: {"object_identifier":"queryid:NNN","severity":"info",` +
	`"rationale":"...","recommended_sql":null,"original_query":"...",` +
	`"suggested_rewrite":"...","expected_improvement":"...",` +
	`"impact_rating":"high|medium|low","requires_app_change":true|false}`

func analyzeQueryRewrites(
	ctx context.Context,
	pool *pgxpool.Pool,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	// Select candidate queries: top by total time, or high temp writes.
	type candidate struct {
		query  collector.QueryStats
		reason string
	}
	var candidates []candidate

	// Top 10 by total exec time.
	sorted := make([]collector.QueryStats, len(snap.Queries))
	copy(sorted, snap.Queries)

	// Simple sort by total time desc.
	for i := 0; i < len(sorted) && i < 10; i++ {
		maxIdx := i
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].TotalExecTime > sorted[maxIdx].TotalExecTime {
				maxIdx = j
			}
		}
		sorted[i], sorted[maxIdx] = sorted[maxIdx], sorted[i]
	}

	for i := 0; i < len(sorted) && i < 10; i++ {
		q := sorted[i]
		if q.Calls < 100 || q.MeanExecTime < 50 {
			continue
		}
		candidates = append(candidates,
			candidate{q, "high total time"},
		)
	}

	// Queries with temp spills.
	for _, q := range snap.Queries {
		if q.TempBlksWritten > 0 && q.Calls > 50 {
			candidates = append(candidates,
				candidate{q, "temp spills"},
			)
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Deduplicate by queryid.
	seen := make(map[int64]bool)
	var unique []candidate
	for _, c := range candidates {
		if !seen[c.query.QueryID] {
			seen[c.query.QueryID] = true
			unique = append(unique, c)
		}
	}
	if len(unique) > 10 {
		unique = unique[:10]
	}

	// Per-query dedup: skip candidates that already have an open
	// query_rewrite finding in sage.findings. The object_identifier
	// is stored as "queryid:NNN" so we extract known queryids.
	if pool != nil {
		existing := openRewriteQueryIDs(ctx, pool, logFn)
		if len(existing) > 0 {
			var filtered []candidate
			for _, c := range unique {
				if existing[c.query.QueryID] {
					logFn("DEBUG",
						"advisor: rewrite: skipping queryid %d, open finding exists",
						c.query.QueryID)
					continue
				}
				filtered = append(filtered, c)
			}
			unique = filtered
		}
	}

	if len(unique) == 0 {
		return nil, nil
	}

	var queryLines []string
	for _, c := range unique {
		q := c.query
		truncQuery := llm.SanitizeForLLM(q.Query)
		if len(truncQuery) > 300 {
			truncQuery = truncQuery[:300] + "..."
		}
		queryLines = append(queryLines, fmt.Sprintf(
			"Query (queryid=%d, reason=%s):\n  %s\n"+
				"  calls=%d mean_exec=%.1fms rows=%d "+
				"shared_blks_read=%d temp_blks_written=%d",
			q.QueryID, c.reason, truncQuery,
			q.Calls, q.MeanExecTime, q.Rows,
			q.SharedBlksRead, q.TempBlksWritten,
		))
	}

	prompt := fmt.Sprintf(
		"QUERY REWRITE CONTEXT:\n\n%s",
		strings.Join(queryLines, "\n\n"),
	)

	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}

	resp, _, err := mgr.ChatForPurpose(
		ctx, "advisor", rewriteSystemPrompt, prompt, 4096,
	)
	if err != nil {
		return nil, fmt.Errorf("rewrite LLM: %w", err)
	}

	parsed := parseLLMFindings(resp, "query_rewrite", logFn)
	// Filter out CREATE INDEX suggestions — those belong in the
	// optimizer, not query_rewrite. Check both suggested_rewrite
	// and recommended_sql in the detail map.
	var findings []analyzer.Finding
	for _, f := range parsed {
		if isCreateIndexRewrite(f) {
			continue
		}
		// Rewrites are advisory suggestions for developers, never
		// auto-executed (see system prompt). The prompt asks the LLM
		// for severity "info"; force it here so a mis-behaving model
		// cannot escalate these into operator-actionable warnings.
		f.Severity = "info"
		f.RecommendedSQL = ""
		f.ActionRisk = ""
		findings = append(findings, f)
	}
	return findings, nil
}

// openRewriteQueryIDs returns the set of queryids that already have
// open query_rewrite findings. The object_identifier is stored as
// "queryid:NNN", so we parse the numeric suffix.
func openRewriteQueryIDs(
	ctx context.Context,
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) map[int64]bool {
	rows, err := pool.Query(ctx,
		`SELECT object_identifier FROM sage.findings
		 WHERE category = 'query_rewrite'
		   AND status = 'open'
		   AND acted_on_at IS NULL`,
	)
	if err != nil {
		logFn("WARN",
			"advisor: rewrite dedup query failed: %v", err)
		return nil
	}
	defer rows.Close()

	ids := make(map[int64]bool)
	for rows.Next() {
		var ident string
		if err := rows.Scan(&ident); err != nil {
			continue
		}
		after, found := strings.CutPrefix(ident, "queryid:")
		if !found {
			continue
		}
		qid, err := strconv.ParseInt(
			strings.TrimSpace(after), 10, 64,
		)
		if err != nil {
			continue
		}
		ids[qid] = true
	}
	return ids
}

// isCreateIndexRewrite returns true if the finding's suggested
// rewrite or recommended_sql is a CREATE INDEX statement. These
// overlap with optimizer recommendations and should be filtered.
func isCreateIndexRewrite(f analyzer.Finding) bool {
	if f.Detail == nil {
		return false
	}
	for _, key := range []string{
		"suggested_rewrite", "recommended_sql",
	} {
		val, _ := f.Detail[key].(string)
		if val == "" {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(val))
		if strings.HasPrefix(upper, "CREATE INDEX") ||
			strings.HasPrefix(upper, "CREATE UNIQUE INDEX") {
			return true
		}
	}
	return false
}
