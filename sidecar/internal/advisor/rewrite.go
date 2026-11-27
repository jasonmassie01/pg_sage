package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/workload"
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
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	unique := selectRewriteCandidates(snap)
	if len(unique) == 0 {
		return nil, nil
	}

	// Per-query gate: a queryid with an open query_rewrite finding is not
	// asked about again while that finding is open (advisors-02).
	gate := openGateFrom(ctx)
	var filtered []rewriteCandidate
	for _, c := range unique {
		if gate.queryIDs[c.query.QueryID] {
			logFn("DEBUG", "advisor: rewrite: skipping queryid %d, open finding exists",
				c.query.QueryID)
			continue
		}
		filtered = append(filtered, c)
	}
	unique = filtered

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

	resp, err := chatAdvisor(ctx, mgr, "rewrite", rewriteSystemPrompt, "", prompt)
	if err != nil {
		return nil, err
	}

	return rewriteFindings(parseLLMFindings(resp, "query_rewrite", logFn)), nil
}

// rewriteFindings keeps the parsed rewrite suggestions as advisory
// findings. CREATE INDEX suggestions are dropped: those belong in the
// optimizer, not query_rewrite.
func rewriteFindings(parsed []analyzer.Finding) []analyzer.Finding {
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
	return findings
}

// rewriteCandidate is a statement offered to the rewrite advisor, with
// why it was picked.
type rewriteCandidate struct {
	query  collector.QueryStats
	reason string
}

// selectRewriteCandidates picks at most 10 workload statements
// (internal/workload): the heaviest by total time among the top 10, then
// those spilling to temp files, each once.
func selectRewriteCandidates(snap *collector.Snapshot) []rewriteCandidate {
	queries := workload.Queries(snap.Queries)
	sort.SliceStable(queries, func(i, j int) bool {
		return queries[i].TotalExecTime > queries[j].TotalExecTime
	})
	var candidates []rewriteCandidate
	for i := 0; i < len(queries) && i < 10; i++ {
		if q := queries[i]; q.Calls >= 100 && q.MeanExecTime >= 50 {
			candidates = append(candidates, rewriteCandidate{q, "high total time"})
		}
	}
	for _, q := range queries {
		if q.TempBlksWritten > 0 && q.Calls > 50 {
			candidates = append(candidates, rewriteCandidate{q, "temp spills"})
		}
	}
	seen := make(map[int64]bool)
	var unique []rewriteCandidate
	for _, c := range candidates {
		if !seen[c.query.QueryID] && len(unique) < 10 {
			seen[c.query.QueryID] = true
			unique = append(unique, c)
		}
	}
	return unique
}

// isCreateIndexRewrite returns true if the finding's SQL, or the
// suggested_rewrite or recommended_sql in its detail, is a CREATE INDEX
// statement. These overlap with optimizer recommendations and should be
// filtered; reading RecommendedSQL keeps the filter independent of the
// detail layout.
func isCreateIndexRewrite(f analyzer.Finding) bool {
	suggested, _ := f.Detail["suggested_rewrite"].(string)
	recommended, _ := f.Detail["recommended_sql"].(string)
	for _, val := range []string{f.RecommendedSQL, suggested, recommended} {
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
