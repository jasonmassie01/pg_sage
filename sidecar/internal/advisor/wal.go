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

const walSystemPrompt = `You are a PostgreSQL WAL and checkpoint tuning expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. If checkpoints_req > 20% of total, max_wal_size is too small.
2. If buffers_backend > 5% of total buffers written, checkpoints can't keep up.
3. Recommend specific values for max_wal_size based on WAL generation rate.
4. For managed services, only recommend adjustable settings.
5. Show the math: current WAL between checkpoints, recommended max_wal_size.
6. If everything is healthy (< 5% forced checkpoints), return [].
7. All recommendations are ALTER SYSTEM SET.
8. Flag if pending_restart = true.
9. Never recommend reducing max_wal_size below current.

Each element: {"object_identifier":"instance","severity":"info",` +
	`"rationale":"...","recommended_sql":"ALTER SYSTEM SET ...",` +
	`"current_settings":{...},"recommended_settings":{...},` +
	`"requires_restart":false}`

func analyzeWAL(
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

	// Build WAL settings context.
	var walSettings []string
	for _, s := range snap.ConfigData.PGSettings {
		switch s.Name {
		case "max_wal_size", "min_wal_size",
			"checkpoint_completion_target",
			"wal_compression", "wal_level", "wal_buffers",
			"checkpoint_timeout", "full_page_writes":
			restart := ""
			if s.PendingRestart {
				restart = " (pending_restart=true)"
			}
			walSettings = append(walSettings,
				fmt.Sprintf("  %s = %s%s%s",
					s.Name, s.Setting, s.Unit, restart),
			)
		}
	}

	// Checkpoint metrics from system stats.
	sys := snap.System
	totalCP := sys.TotalCheckpoints
	var prevCP int64
	if prev != nil {
		prevCP = prev.System.TotalCheckpoints
	}
	cpDelta := totalCP - prevCP

	// Detect platform.
	platform := detectPlatform(snap.ConfigData.PGSettings)

	// Count unlogged tables so the LLM knows some tables don't generate WAL.
	unloggedNote := ""
	unloggedCount := countUnloggedTables(snap)
	if unloggedCount > 0 {
		unloggedNote = fmt.Sprintf(
			"\n\nNote: %d unlogged table(s) detected. "+
				"Unlogged tables do NOT generate WAL, so low WAL "+
				"activity may be expected if writes target them.",
			unloggedCount,
		)
	}

	prompt := fmt.Sprintf(
		"WAL & CHECKPOINT CONTEXT:\n\n"+
			"Current settings:\n%s\n\n"+
			"Observed metrics:\n"+
			"  Total checkpoints: %d (delta since last: %d)\n"+
			"  WAL position: %s\n\n"+
			"Platform: %s%s",
		strings.Join(walSettings, "\n"),
		totalCP, cpDelta,
		snap.ConfigData.WALPosition,
		platform,
		unloggedNote,
	)

	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}

	resp, _, err := mgr.ChatForPurpose(
		ctx, "advisor", walSystemPrompt, prompt, 4096,
	)
	if err != nil {
		return nil, fmt.Errorf("wal LLM: %w", err)
	}

	return parseLLMFindings(resp, "wal_tuning", logFn), nil
}

// countUnloggedTables returns the number of tables with relpersistence='u'.
func countUnloggedTables(snap *collector.Snapshot) int {
	count := 0
	for _, t := range snap.Tables {
		if t.IsUnlogged() {
			count++
		}
	}
	return count
}

// detectPlatform infers managed service from pg_settings.
func detectPlatform(settings []collector.PGSetting) string {
	for _, s := range settings {
		if s.Source == "cloud-sql" ||
			strings.Contains(s.Source, "cloudsql") {
			return "cloud-sql"
		}
	}
	return "self-managed"
}
