package advisor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const connectionSystemPrompt = `You are a PostgreSQL connection management expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. max_connections caps the number of OPEN connections (active AND idle),
   not just active ones. Size every recommendation against TOTAL backends.
   NEVER size against active-only counts.
2. Only recommend reducing max_connections if it exceeds ~4x the peak TOTAL
   connections AND there is comfortable headroom; reduce conservatively.
3. NEVER recommend max_connections below
   max(current total backends, peak total backends) + superuser_reserved_connections
   + a safety margin of at least 10. Cutting below current usage rejects live
   connections — this is a hard rule.
4. If new connections/minute > 10 with short duration, recommend PgBouncer.
5. If idle_in_transaction > 60s average, recommend ` +
	`idle_in_transaction_session_timeout.
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

	findings := parseLLMFindings(resp, "connection_tuning", logFn)
	return filterUnsafeMaxConnections(findings, sys, logFn), nil
}

// filterUnsafeMaxConnections drops any recommendation that would set
// max_connections below the number of connections already open (plus
// reserved slots and a safety margin). The LLM is instructed to size
// against total backends, but this is the hard deterministic backstop:
// cutting below current usage would reject live connections.
func filterUnsafeMaxConnections(
	findings []analyzer.Finding,
	sys collector.SystemStats,
	logFn func(string, string, ...any),
) []analyzer.Finding {
	// superuser_reserved_connections defaults to 3; the margin covers it
	// plus headroom for spikes and pg_sage's own pool.
	const reservedAndMargin = 13
	floor := sys.TotalBackends + reservedAndMargin
	out := findings[:0]
	for _, f := range findings {
		if n, ok := parseMaxConnectionsTarget(f.RecommendedSQL); ok && n < floor {
			if logFn != nil {
				logFn("advisor",
					"rejecting max_connections=%d: below safe floor %d "+
						"(total backends %d + reserved/margin %d)",
					n, floor, sys.TotalBackends, reservedAndMargin)
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// parseMaxConnectionsTarget extracts N from an
// "ALTER SYSTEM SET max_connections = N" statement.
func parseMaxConnectionsTarget(sql string) (int, bool) {
	low := strings.ToLower(sql)
	i := strings.Index(low, "max_connections")
	if i < 0 {
		return 0, false
	}
	rest := sql[i+len("max_connections"):]
	rest = strings.TrimLeft(rest, " \t=")
	rest = strings.Trim(strings.TrimSpace(rest), "'\";")
	digits := strings.Builder{}
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		digits.WriteRune(r)
	}
	if digits.Len() == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(digits.String())
	if err != nil {
		return 0, false
	}
	return n, true
}
