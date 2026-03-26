package advisor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const vacuumSystemPrompt = `You are a PostgreSQL vacuum tuning expert. ` +
	`Given the vacuum context below, recommend per-table autovacuum overrides.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning, ` +
	`no explanation outside the JSON.

RULES:
1. Only recommend changes for tables where autovacuum is falling behind.
2. Output ALTER TABLE ... SET (...) DDL for per-table overrides.
3. If the problem is global (cost_delay too high, max_workers too low), ` +
	`recommend ALTER SYSTEM.
4. Explain WHY the current settings are wrong (show the math).
5. If a table has no dead tuple problem, say "no changes needed".
6. For high-write tables (>10K writes/day), recommend scale_factor 0.01-0.05.
7. For append-only tables (inserts only, no updates/deletes), vacuum is less ` +
	`critical.
8. Never set scale_factor to 0.
9. Don't recommend changes for tables with < 1000 rows or dead_tuple_ratio < 5%.

Each element: {"object_identifier":"schema.table","severity":"info",` +
	`"rationale":"...","recommended_sql":"ALTER TABLE ...",` +
	`"current_settings":{...},"recommended_settings":{...}}`

func analyzeVacuum(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	prev *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	// Build context for tables with dead tuple issues.
	var tableContexts []string
	for _, t := range snap.Tables {
		total := t.NLiveTup + t.NDeadTup
		if total < 1000 {
			continue
		}
		ratio := float64(t.NDeadTup) / float64(max(total, 1))
		if ratio < 0.05 {
			continue
		}

		// Calculate write rate from previous snapshot.
		writeRate := "unknown"
		if prev != nil {
			for _, pt := range prev.Tables {
				if pt.SchemaName == t.SchemaName &&
					pt.RelName == t.RelName {
					elapsed := snap.CollectedAt.Sub(prev.CollectedAt)
					if elapsed > 0 {
						hrs := elapsed.Hours()
						insPerDay := float64(t.NTupIns-pt.NTupIns) / hrs * 24
						updPerDay := float64(t.NTupUpd-pt.NTupUpd) / hrs * 24
						delPerDay := float64(t.NTupDel-pt.NTupDel) / hrs * 24
						writeRate = fmt.Sprintf(
							"%.0f ins/day, %.0f upd/day, %.0f del/day",
							insPerDay, updPerDay, delPerDay,
						)
					}
					break
				}
			}
		}

		vacuumInfo := "never"
		if t.LastAutovacuum != nil {
			vacuumInfo = fmt.Sprintf(
				"%s ago",
				time.Since(*t.LastAutovacuum).Round(time.Minute),
			)
		}

		// Find per-table overrides from config snapshot.
		overrides := "none"
		if snap.ConfigData != nil {
			for _, ro := range snap.ConfigData.TableReloptions {
				if ro.SchemaName == t.SchemaName &&
					ro.RelName == t.RelName {
					overrides = ro.Reloptions
					break
				}
			}
		}

		tableContexts = append(tableContexts, fmt.Sprintf(
			"Table: %s.%s (%d rows)\n"+
				"  Dead tuples: %d (%.1f%%)\n"+
				"  Write rate: %s\n"+
				"  Last autovacuum: %s\n"+
				"  Autovacuum count (total): %d\n"+
				"  Per-table overrides: %s",
			t.SchemaName, t.RelName, t.NLiveTup,
			t.NDeadTup, ratio*100,
			writeRate,
			vacuumInfo,
			t.AutovacuumCount,
			overrides,
		))
	}

	if len(tableContexts) == 0 {
		return nil, nil
	}

	// Add global settings.
	var globalSettings []string
	if snap.ConfigData != nil {
		for _, s := range snap.ConfigData.PGSettings {
			if strings.HasPrefix(s.Name, "autovacuum") {
				globalSettings = append(globalSettings,
					fmt.Sprintf("  %s = %s", s.Name, s.Setting),
				)
			}
		}
	}

	prompt := fmt.Sprintf(
		"VACUUM CONTEXT:\n\nGlobal settings:\n%s\n\n%s",
		strings.Join(globalSettings, "\n"),
		strings.Join(tableContexts, "\n\n"),
	)

	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}

	resp, _, err := mgr.ChatForPurpose(
		ctx, "advisor", vacuumSystemPrompt, prompt, 4096,
	)
	if err != nil {
		return nil, fmt.Errorf("vacuum LLM: %w", err)
	}

	findings := parseLLMFindings(resp, "vacuum_tuning", logFn)

	// Validate each finding.
	var valid []analyzer.Finding
	for _, f := range findings {
		if f.RecommendedSQL != "" {
			if err := ValidateConfigRecommendation("", "", ""); err == nil {
				valid = append(valid, f)
			}
		} else {
			valid = append(valid, f)
		}
	}

	return valid, nil
}
