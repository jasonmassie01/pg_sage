package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const bloatSystemPrompt = `You are a PostgreSQL bloat remediation expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. Present multiple options (VACUUM FULL, pg_repack, do nothing) with tradeoffs.
2. If pg_repack is available, prefer it over VACUUM FULL for production.
3. Estimate duration: ~1MB/s VACUUM FULL, ~0.5MB/s pg_repack.
4. Calculate temp disk space for pg_repack.
5. If bloat < 20% and stable, recommend "do nothing -- monitor."
6. For managed services without pg_repack: VACUUM FULL needs maintenance window.
7. Suggest maintenance window from lowest-traffic period.
8. For tables > 10GB, warn about VACUUM FULL duration.
9. For index bloat, recommend REINDEX CONCURRENTLY.
10. These are ALWAYS advisory -- severity: info. Never auto-execute.

Each element: {"object_identifier":"schema.table","severity":"info",` +
	`"rationale":"...","recommended_sql":null,"bloat_pct":N,` +
	`"remediation_options":[...]}`

func analyzeBloat(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	prev *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}

	// Find bloated tables.
	var bloatContexts []string
	for _, t := range snap.Tables {
		total := t.NLiveTup + t.NDeadTup
		if total < 1000 {
			continue
		}
		ratio := float64(t.NDeadTup) / float64(max(total, 1))
		if ratio < 0.10 { // Only look at 10%+ bloat for remediation
			continue
		}

		sizeMB := float64(t.TableBytes) / (1024 * 1024)
		indexSizeMB := float64(t.IndexBytes) / (1024 * 1024)
		estBloatMB := sizeMB * ratio

		// Check if bloat is growing or stable.
		trend := "unknown"
		if prev != nil {
			for _, pt := range prev.Tables {
				if pt.SchemaName == t.SchemaName &&
					pt.RelName == t.RelName {
					prevTotal := max(pt.NLiveTup+pt.NDeadTup, 1)
					prevRatio := float64(pt.NDeadTup) / float64(prevTotal)
					if ratio > prevRatio+0.02 {
						trend = "growing"
					} else if ratio < prevRatio-0.02 {
						trend = "shrinking"
					} else {
						trend = "stable"
					}
					break
				}
			}
		}

		vacuumInfo := "never"
		if t.LastAutovacuum != nil {
			vacuumInfo = t.LastAutovacuum.Format("2006-01-02 15:04")
		}

		bloatContexts = append(bloatContexts, fmt.Sprintf(
			"Table: %s.%s\n"+
				"  Size: %.1fMB (data) + %.1fMB (indexes)\n"+
				"  Dead tuples: %d (%.1f%%)\n"+
				"  Live tuples: %d\n"+
				"  Estimated bloat: %.1fMB\n"+
				"  Bloat trend: %s\n"+
				"  Last autovacuum: %s",
			t.SchemaName, t.RelName,
			sizeMB, indexSizeMB,
			t.NDeadTup, ratio*100,
			t.NLiveTup,
			estBloatMB,
			trend,
			vacuumInfo,
		))
	}

	if len(bloatContexts) == 0 {
		return nil, nil
	}

	// Check pg_repack availability.
	hasRepack := false
	for _, ext := range snap.ConfigData.ExtensionsAvailable {
		if ext == "pg_repack" {
			hasRepack = true
			break
		}
	}

	platform := detectPlatform(snap.ConfigData.PGSettings)

	prompt := fmt.Sprintf(
		"BLOAT REMEDIATION CONTEXT:\n\n"+
			"pg_repack available: %v\n"+
			"Platform: %s\n"+
			"Database size: %d bytes\n\n%s",
		hasRepack,
		platform,
		snap.System.DBSizeBytes,
		strings.Join(bloatContexts, "\n\n"),
	)

	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}

	resp, _, err := mgr.ChatForPurpose(
		ctx, "advisor", bloatSystemPrompt, prompt, 4096,
	)
	if err != nil {
		return nil, fmt.Errorf("bloat LLM: %w", err)
	}

	findings := parseLLMFindings(resp, "bloat_remediation", logFn)
	for i := range findings {
		findings[i].Severity = "info"
		findings[i].RecommendedSQL = ""
	}
	return findings, nil
}
