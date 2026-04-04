package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// cronSchedule holds pre-parsed bitmasks for each cron field.
type cronSchedule struct {
	minutes [60]bool
	hours   [24]bool
	doms    [31]bool // 0-indexed: doms[0] = day 1
	months  [12]bool // 0-indexed: months[0] = January
	dows    [7]bool  // 0 = Sunday
	valid   bool
}

// matches returns true if time t falls within the schedule.
func (s *cronSchedule) matches(t time.Time) bool {
	return s.minutes[t.Minute()] && s.hours[t.Hour()] &&
		s.doms[t.Day()-1] && s.months[t.Month()-1] &&
		s.dows[t.Weekday()]
}

// parseCron parses a 5-field cron expression (min hour dom month dow).
func parseCron(expr string) (cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return cronSchedule{}, fmt.Errorf(
			"expected 5 fields, got %d", len(fields),
		)
	}
	var s cronSchedule
	var err error
	if err = parseCronField(fields[0], 0, 59, s.minutes[:]); err != nil {
		return cronSchedule{}, fmt.Errorf("minute: %w", err)
	}
	if err = parseCronField(fields[1], 0, 23, s.hours[:]); err != nil {
		return cronSchedule{}, fmt.Errorf("hour: %w", err)
	}
	if err = parseCronField(fields[2], 1, 31, s.doms[:]); err != nil {
		return cronSchedule{}, fmt.Errorf("dom: %w", err)
	}
	if err = parseCronField(fields[3], 1, 12, s.months[:]); err != nil {
		return cronSchedule{}, fmt.Errorf("month: %w", err)
	}
	if err = parseCronField(fields[4], 0, 6, s.dows[:]); err != nil {
		return cronSchedule{}, fmt.Errorf("dow: %w", err)
	}
	s.valid = true
	return s, nil
}

// parseCronField parses one cron field into a bool slice.
// The slice is indexed from 0; min is subtracted for 1-based fields.
func parseCronField(field string, min, max int, out []bool) error {
	for _, part := range strings.Split(field, ",") {
		if err := parseCronPart(part, min, max, out); err != nil {
			return err
		}
	}
	return nil
}

// parseCronPart handles a single element: *, N, N-M, */S, N-M/S.
func parseCronPart(part string, min, max int, out []bool) error {
	step := 1
	rangePart := part
	if idx := strings.Index(part, "/"); idx >= 0 {
		var err error
		step, err = strconv.Atoi(part[idx+1:])
		if err != nil || step <= 0 {
			return fmt.Errorf("invalid step in %q", part)
		}
		rangePart = part[:idx]
	}
	lo, hi := min, max
	if rangePart != "*" {
		if dash := strings.Index(rangePart, "-"); dash >= 0 {
			var err error
			lo, err = strconv.Atoi(rangePart[:dash])
			if err != nil {
				return fmt.Errorf("invalid range start in %q", part)
			}
			hi, err = strconv.Atoi(rangePart[dash+1:])
			if err != nil {
				return fmt.Errorf("invalid range end in %q", part)
			}
		} else {
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return fmt.Errorf("invalid value %q", rangePart)
			}
			lo, hi = v, v
		}
	}
	if lo < min || hi > max || lo > hi {
		return fmt.Errorf("out of range [%d-%d] in %q", min, max, part)
	}
	for i := lo; i <= hi; i += step {
		out[i-min] = true
	}
	return nil
}

// Worker generates periodic health briefings.
type Worker struct {
	pool     *pgxpool.Pool
	cfg      *config.Config
	llm      *llm.Client
	logFn    func(string, string, ...any)
	lastRun  time.Time
	schedule cronSchedule
}

// New creates a briefing worker.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	llmClient *llm.Client,
	logFn func(string, string, ...any),
) *Worker {
	sched, err := parseCron(cfg.Briefing.Schedule)
	if err != nil {
		logFn("WARN", "briefing",
			"invalid schedule %q: %v", cfg.Briefing.Schedule, err)
	}
	return &Worker{
		pool:     pool,
		cfg:      cfg,
		llm:      llmClient,
		logFn:    logFn,
		schedule: sched,
	}
}

// ShouldRun returns true when now matches the cron schedule and at
// least 30 seconds have elapsed since the last run.
func (w *Worker) ShouldRun(now time.Time) bool {
	if !w.schedule.valid {
		return false
	}
	if !w.lastRun.IsZero() && now.Sub(w.lastRun) < 30*time.Second {
		return false
	}
	return w.schedule.matches(now)
}

// MarkRan records that a briefing was just generated.
func (w *Worker) MarkRan() {
	w.lastRun = time.Now()
}

// maxBriefingFindings caps findings sent to the LLM prompt.
const maxBriefingFindings = 50

// Generate creates a briefing from current findings and system state.
func (w *Worker) Generate(ctx context.Context) (string, error) {
	// Gather data.
	findings, totalOpen, err := w.gatherFindings(ctx)
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
	structured := w.buildStructured(findings, totalOpen, system, actions)

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

func (w *Worker) gatherFindings(ctx context.Context) (string, int, error) {
	var result string
	var totalOpen int
	err := w.pool.QueryRow(ctx, `
		SELECT coalesce(
			(SELECT json_agg(t) FROM (
				SELECT
					category,
					severity,
					title,
					object_identifier,
					occurrence_count,
					recommended_sql
				FROM sage.findings
				WHERE status = 'open'
				ORDER BY
					CASE severity
						WHEN 'critical' THEN 0
						WHEN 'warning'  THEN 1
						ELSE 2
					END,
					occurrence_count DESC
				LIMIT $1
			) t),
			'[]'::json
		)::text,
		coalesce(
			(SELECT count(*) FROM sage.findings WHERE status = 'open'),
			0
		)
	`, maxBriefingFindings).Scan(&result, &totalOpen)
	return result, totalOpen, err
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

func (w *Worker) buildStructured(findings string, totalOpen int, system, actions string) string {
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
		shown := len(findingsList)
		if totalOpen > shown {
			b.WriteString(fmt.Sprintf(
				"## Findings (%d of %d open): %d critical, %d warning, %d info\n\n",
				shown, totalOpen, critical, warning, info))
		} else {
			b.WriteString(fmt.Sprintf(
				"## Findings: %d critical, %d warning, %d info\n\n",
				critical, warning, info))
		}

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
