package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Worker generates periodic health briefings.
type Worker struct {
	pool         *pgxpool.Pool
	cfg          *config.Config
	llm          *llm.Client
	logFn        func(string, string, ...any)
	lastRun      time.Time
	scheduleHour int // hour of day to run (from cron), -1 if unparsed
}

// New creates a briefing worker.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	llmClient *llm.Client,
	logFn func(string, string, ...any),
) *Worker {
	return &Worker{
		pool:         pool,
		cfg:          cfg,
		llm:          llmClient,
		logFn:        logFn,
		scheduleHour: parseScheduleHour(cfg.Briefing.Schedule),
	}
}

// parseScheduleHour extracts the hour from a cron expression like
// "0 6 * * *". Returns -1 if the format is unexpected.
func parseScheduleHour(cron string) int {
	parts := strings.Fields(cron)
	if len(parts) < 2 {
		return -1
	}
	h := 0
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return -1
		}
		h = h*10 + int(c-'0')
	}
	if h > 23 {
		return -1
	}
	return h
}

// ShouldRun returns true when enough time has elapsed since the last
// briefing and the scheduled hour has arrived (or been passed).
func (w *Worker) ShouldRun(now time.Time) bool {
	// Never ran — run if we're past the scheduled hour today.
	if w.lastRun.IsZero() {
		if w.scheduleHour < 0 {
			return false
		}
		return now.Hour() >= w.scheduleHour
	}
	// Already ran today.
	if w.lastRun.Year() == now.Year() &&
		w.lastRun.YearDay() == now.YearDay() {
		return false
	}
	// New day — run if past scheduled hour.
	if w.scheduleHour < 0 {
		return false
	}
	return now.Hour() >= w.scheduleHour
}

// MarkRan records that a briefing was just generated.
func (w *Worker) MarkRan() {
	w.lastRun = time.Now()
}

// Generate creates a briefing from current findings and system state.
func (w *Worker) Generate(ctx context.Context) (string, error) {
	// Gather data.
	findings, err := w.gatherFindings(ctx)
	if err != nil {
		return "", fmt.Errorf("gather findings: %w", err)
	}

	system, err := w.gatherSystem(ctx)
	if err != nil {
		return "", fmt.Errorf("gather system: %w", err)
	}

	actions, err := w.gatherRecentActions(ctx)
	if err != nil {
		actions = "[]" // non-fatal
	}

	// Build structured briefing.
	structured := w.buildStructured(findings, system, actions)

	// If LLM enabled, enhance with natural language.
	if w.llm != nil && w.llm.IsEnabled() && !w.llm.IsCircuitOpen() {
		enhanced, tokens, err := w.enhanceWithLLM(ctx, structured)
		if err != nil {
			w.logFn("WARN", "briefing", "LLM enhancement failed: %v, using structured", err)
		} else {
			w.storeBriefing(ctx, enhanced, true, tokens)
			return enhanced, nil
		}
	}

	w.storeBriefing(ctx, structured, false, 0)
	return structured, nil
}

func (w *Worker) gatherFindings(ctx context.Context) (string, error) {
	var result string
	err := w.pool.QueryRow(ctx, `
		SELECT coalesce(
			(SELECT json_agg(json_build_object(
				'category', category,
				'severity', severity,
				'title', title,
				'object_identifier', object_identifier,
				'occurrence_count', occurrence_count,
				'recommended_sql', recommended_sql
			) ORDER BY
				CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
				occurrence_count DESC
			)
			FROM sage.findings WHERE status = 'open'),
			'[]'::json
		)::text
	`).Scan(&result)
	return result, err
}

func (w *Worker) gatherSystem(ctx context.Context) (string, error) {
	var result string
	err := w.pool.QueryRow(ctx, `
		SELECT json_build_object(
			'db_size', pg_size_pretty(pg_database_size(current_database())),
			'connections', (SELECT count(*) FROM pg_stat_activity),
			'active', (SELECT count(*) FROM pg_stat_activity WHERE state = 'active'),
			'cache_hit_ratio', (
				SELECT round((blks_hit::numeric / nullif(blks_hit + blks_read, 0) * 100), 2)
				FROM pg_stat_database WHERE datname = current_database()
			),
			'uptime_hours', (
				SELECT extract(epoch FROM now() - pg_postmaster_start_time()) / 3600
			)::int
		)::text
	`).Scan(&result)
	return result, err
}

func (w *Worker) gatherRecentActions(ctx context.Context) (string, error) {
	var result string
	err := w.pool.QueryRow(ctx, `
		SELECT coalesce(
			(SELECT json_agg(json_build_object(
				'action_type', action_type,
				'outcome', outcome,
				'executed_at', executed_at
			) ORDER BY executed_at DESC)
			FROM sage.action_log
			WHERE executed_at > now() - interval '24 hours'),
			'[]'::json
		)::text
	`).Scan(&result)
	return result, err
}

func (w *Worker) buildStructured(findings, system, actions string) string {
	var b strings.Builder
	now := time.Now().Format("2006-01-02 15:04 MST")

	b.WriteString(fmt.Sprintf("# pg_sage Health Briefing — %s\n\n", now))

	// System overview.
	var sys map[string]any
	if err := json.Unmarshal([]byte(system), &sys); err == nil {
		b.WriteString("## System Overview\n")
		for k, v := range sys {
			b.WriteString(fmt.Sprintf("- **%s**: %v\n", k, v))
		}
		b.WriteString("\n")
	}

	// Findings summary.
	var findingsList []map[string]any
	if err := json.Unmarshal([]byte(findings), &findingsList); err == nil {
		critical, warning, info := 0, 0, 0
		for _, f := range findingsList {
			switch f["severity"] {
			case "critical":
				critical++
			case "warning":
				warning++
			default:
				info++
			}
		}
		b.WriteString(fmt.Sprintf("## Findings: %d critical, %d warning, %d info\n\n",
			critical, warning, info))

		for _, f := range findingsList {
			sev := f["severity"]
			icon := "ℹ️"
			if sev == "critical" {
				icon = "🔴"
			} else if sev == "warning" {
				icon = "🟡"
			}
			b.WriteString(fmt.Sprintf("%s **%s** — %s", icon, f["severity"], f["title"]))
			if obj, ok := f["object_identifier"]; ok && obj != nil {
				b.WriteString(fmt.Sprintf(" (`%s`)", obj))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Recent actions.
	var actionsList []map[string]any
	if err := json.Unmarshal([]byte(actions), &actionsList); err == nil && len(actionsList) > 0 {
		b.WriteString("## Recent Actions (24h)\n")
		for _, a := range actionsList {
			b.WriteString(fmt.Sprintf("- %s → %s\n", a["action_type"], a["outcome"]))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (w *Worker) enhanceWithLLM(ctx context.Context, structured string) (string, int, error) {
	system := `You are pg_sage, a PostgreSQL DBA agent. Generate a concise health briefing
from the structured data provided. Use markdown. Be actionable and specific.
Prioritize critical findings. Keep it under 2000 words.`

	return w.llm.Chat(ctx, system, structured, w.cfg.LLM.ContextBudgetTokens)
}

func (w *Worker) storeBriefing(ctx context.Context, content string, llmUsed bool, tokens int) {
	now := time.Now()
	_, err := w.pool.Exec(ctx, `
		INSERT INTO sage.briefings (generated_at, period_start, period_end, mode, content_text, content_json, llm_used, token_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, now, now.Add(-24*time.Hour), now, "executive", content,
		json.RawMessage(`{}`), llmUsed, tokens)
	if err != nil {
		w.logFn("WARN", "briefing", "failed to store briefing: %v", err)
	}
}

// Dispatch sends the briefing to configured channels.
func (w *Worker) Dispatch(briefing string) {
	for _, ch := range w.cfg.Briefing.Channels {
		switch ch {
		case "stdout":
			fmt.Println(briefing)
		case "slack":
			if w.cfg.Briefing.SlackWebhookURL != "" {
				w.sendSlack(briefing)
			}
		}
	}
}

func (w *Worker) sendSlack(text string) {
	payload, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", w.cfg.Briefing.SlackWebhookURL,
		strings.NewReader(string(payload)))
	if err != nil {
		w.logFn("WARN", "briefing", "slack request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.logFn("WARN", "briefing", "slack send error: %v", err)
		return
	}
	resp.Body.Close()
}
