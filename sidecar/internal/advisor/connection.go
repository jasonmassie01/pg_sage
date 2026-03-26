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

const connectionSystemPrompt = `You are a PostgreSQL connection management expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. If max_connections > 4x peak active, recommend reducing.
2. If new connections/minute > 10 with short duration, recommend PgBouncer.
3. If idle_in_transaction > 60s average, recommend ` +
	`idle_in_transaction_session_timeout.
4. Calculate memory savings from reducing max_connections.
5. Never recommend max_connections below (peak_active * 1.5 + reserved + 5).
6. If a pooler is detected (from application_name), don't recommend adding one.
7. Recommend specific timeout values.
8. If everything is healthy (utilization < 50%, no long idle tx), return [].

Each element: {"object_identifier":"instance","severity":"info",` +
	`"rationale":"...","recommended_sql":"ALTER SYSTEM SET ...",` +
	`"current_settings":{...},"recommended_settings":{...},` +
	`"peak_active":N,"connection_churn_per_min":N}`

func analyzeConnections(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}

	// Connection settings.
	var connSettings []string
	for _, s := range snap.ConfigData.PGSettings {
		switch s.Name {
		case "max_connections", "superuser_reserved_connections",
			"idle_in_transaction_session_timeout",
			"statement_timeout",
			"tcp_keepalives_idle", "tcp_keepalives_interval":
			connSettings = append(connSettings,
				fmt.Sprintf("  %s = %s%s",
					s.Name, s.Setting, s.Unit),
			)
		}
	}

	// Connection state distribution.
	var stateLines []string
	for _, cs := range snap.ConfigData.ConnectionStates {
		stateLines = append(stateLines,
			fmt.Sprintf("  %s: count=%d, avg_duration=%.0fs",
				cs.State, cs.Count, cs.AvgDurationSeconds),
		)
	}

	churnPerMin := float64(snap.ConfigData.ConnectionChurn) / 5.0

	sys := snap.System
	platform := detectPlatform(snap.ConfigData.PGSettings)

	prompt := fmt.Sprintf(
		"CONNECTION CONTEXT:\n\n"+
			"Settings:\n%s\n\n"+
			"Connection state distribution:\n%s\n\n"+
			"Active backends: %d\n"+
			"Total backends: %d\n"+
			"Max connections: %d\n"+
			"Connection churn: %.1f/min (new connections)\n\n"+
			"Platform: %s",
		strings.Join(connSettings, "\n"),
		strings.Join(stateLines, "\n"),
		sys.ActiveBackends,
		sys.TotalBackends,
		sys.MaxConnections,
		churnPerMin,
		platform,
	)

	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}

	resp, _, err := mgr.ChatForPurpose(
		ctx, "advisor", connectionSystemPrompt, prompt, 4096,
	)
	if err != nil {
		return nil, fmt.Errorf("connection LLM: %w", err)
	}

	return parseLLMFindings(resp, "connection_tuning", logFn), nil
}
