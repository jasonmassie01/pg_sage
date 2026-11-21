package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/workload"
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
	// startedAt anchors the schedule when no briefing was ever stored.
	startedAt     time.Time
	lastRunLoaded bool
	// stdout and stderr are the process streams (nil means os.Stdout and
	// os.Stderr); stdioNotice logs the MCP redirect once per worker.
	stdout, stderr io.Writer
	stdioNotice    sync.Once
	// database is the fleet database name, part of each finding's case key.
	database string
	// live is the current briefing config and schedule, swapped whole by
	// Reconfigure (llm-06); nil only for workers built without New.
	live atomic.Pointer[liveSettings]
	// window is the span of the briefing being generated; lastGenerated is
	// the generated_at of the last stored briefing, zero if it failed.
	window        Window
	lastGenerated time.Time
	// notifier sends the digest through Notifications and inbox counts
	// its Needs you section (ux-02b); nil when not wired.
	notifier Notifier
	inbox    InboxCounter
	// gate lists the review model gate's changes (review-05a); nil: none.
	gate GateNotes
	// weeklyLine is the digest's "N messages, M led to an action" line
	// (ux-09b); nil leaves it out.
	weeklyLine WeeklyLineFunc
}

// WithDatabaseName sets the fleet database the worker briefs, so the
// briefing's finding links open the right case on the Cases page.
func (w *Worker) WithDatabaseName(name string) *Worker {
	w.database = name
	return w
}

// New creates a briefing worker.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	llmClient *llm.Client,
	logFn func(string, string, ...any),
) *Worker {
	sched, err := parseCron(cfg.Briefing.Schedule)
	// An empty schedule turns briefings off (llm-06); it is not invalid.
	if err != nil && strings.TrimSpace(cfg.Briefing.Schedule) != "" {
		logFn("WARN", "briefing: invalid schedule %q: %v", cfg.Briefing.Schedule, err)
	}
	w := &Worker{
		pool:      pool,
		cfg:       cfg,
		llm:       llmClient,
		logFn:     logFn,
		schedule:  sched,
		startedAt: time.Now(),
		stdout:    os.Stdout,
		stderr:    os.Stderr,
	}
	bc := cfg.Briefing
	bc.Channels = append([]string(nil), bc.Channels...)
	w.live.Store(&liveSettings{cfg: bc, schedule: sched})
	return w
}

// maxBriefingFindings caps findings sent to the LLM prompt.
const maxBriefingFindings = 50

// openAdviceSQL selects the open findings the briefing may present: none
// about pg_sage's own statements or diagnostic tooling (internal/workload;
// lifeos 2026-10-04 told the operator to "investigate" an EXPLAIN ANALYZE).
// The analyzer no longer opens such findings and resolves older ones; this
// keeps them out of the LLM input until it has.
var openAdviceSQL = `status = 'open' AND ` + workload.FindingAdviceSQL("detail")

func (w *Worker) gatherFindings(ctx context.Context) (string, int, error) {
	var result string
	var totalOpen int
	err := w.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT coalesce(
			(SELECT json_agg(t) FROM (
				SELECT
					id, category, severity, title, object_type,
					object_identifier, rule_id, occurrence_count,
					recommendation, recommended_sql,
					jsonb_strip_nulls(jsonb_build_object('normalized_query',
						detail->>'normalized_query')) AS detail
				FROM sage.findings
				WHERE `+openAdviceSQL+`
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
			(SELECT count(*) FROM sage.findings WHERE `+openAdviceSQL+`),
			0
		)
	`, maxBriefingFindings).Scan(&result, &totalOpen)
	return result, totalOpen, err
}

func (w *Worker) gatherSystem(ctx context.Context) (string, error) {
	var result string
	err := w.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT json_build_object(
			'db_size', pg_size_pretty(pg_database_size(current_database())),
			'connections', (SELECT count(*) FROM pg_stat_activity),
			'active', (SELECT count(*) FROM pg_stat_activity WHERE state = 'active'
				AND `+selfmonitor.ActivityExclusionSQL("")+`),
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

// maxBriefingActions caps the actions a long window reports.
const maxBriefingActions = 100

// gatherRecentActions reads the actions executed in (win.Start, win.End]:
// consecutive windows neither overlap nor leave a gap (llm-06).
func (w *Worker) gatherRecentActions(ctx context.Context, win Window) (string, error) {
	var result string
	err := w.pool.QueryRow(ctx, `/* pg_sage */
		SELECT coalesce(
			(SELECT json_agg(json_build_object(
				'action_type', action_type,
				'outcome', outcome,
				'executed_at', executed_at
			) ORDER BY executed_at DESC)
			FROM (SELECT action_type, outcome, executed_at
			      FROM sage.action_log
			      WHERE executed_at > $1 AND executed_at <= $2
			      ORDER BY executed_at DESC LIMIT $3) a),
			'[]'::json
		)::text
	`, win.Start, win.End, maxBriefingActions).Scan(&result)
	return result, err
}

// gatherSeverityTotals counts the open findings the briefing may present,
// by severity, over all of them rather than the capped list.
func (w *Worker) gatherSeverityTotals(ctx context.Context) (map[string]int, error) {
	rows, err := w.pool.Query(ctx, `/* pg_sage */
		SELECT severity, count(*) FROM sage.findings WHERE `+openAdviceSQL+`
		GROUP BY severity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var severity string
		var n int
		if err := rows.Scan(&severity, &n); err != nil {
			return nil, err
		}
		out[severity] = n
	}
	return out, rows.Err()
}

func (w *Worker) buildStructured(findings string, totalOpen int, system, actions string) string {
	var b strings.Builder
	now := time.Now().Format("2006-01-02 15:04 MST")

	fmt.Fprintf(&b, "# pg_sage Health Briefing — %s\n\n", now)

	// System overview.
	var sys map[string]any
	if err := json.Unmarshal([]byte(system), &sys); err == nil {
		b.WriteString("## System Overview\n")
		for k, v := range sys {
			fmt.Fprintf(&b, "- **%s**: %v\n", k, v)
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
			fmt.Fprintf(&b,
				"## Findings (%d of %d open): %d critical, %d warning, %d info\n\n",
				shown, totalOpen, critical, warning, info)
		} else {
			fmt.Fprintf(&b,
				"## Findings: %d critical, %d warning, %d info\n\n",
				critical, warning, info)
		}

		for _, f := range findingsList {
			sev := f["severity"]
			icon := "ℹ️"
			switch sev {
			case "critical":
				icon = "🔴"
			case "warning":
				icon = "🟡"
			}
			fmt.Fprintf(&b, "%s **%s** — %s", icon, f["severity"], f["title"])
			if obj, ok := f["object_identifier"]; ok && obj != nil {
				fmt.Fprintf(&b, " (`%s`)", obj)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Recent actions.
	var actionsList []map[string]any
	if err := json.Unmarshal([]byte(actions), &actionsList); err == nil && len(actionsList) > 0 {
		fmt.Fprintf(&b, "## Recent Actions (%s)\n", w.window.label())
		for _, a := range actionsList {
			fmt.Fprintf(&b, "- %s → %s\n", a["action_type"], a["outcome"])
		}
		b.WriteString("\n")
	}

	return b.String()
}

// briefingMaxTokens caps the answer: 2000 words is about 2700 tokens. It
// is not llm.context_budget_tokens, the trim target for attached context.
const briefingMaxTokens = 4096

func (w *Worker) enhanceWithLLM(ctx context.Context, structured string) (string, int, error) {
	system := `You are pg_sage, a PostgreSQL DBA agent. Write a concise health briefing
from the data provided. Use markdown. Prioritize critical findings. Keep it
under 2000 words.

Ground every point in the data. Cite each finding you discuss as "finding #<id>",
using its id. Propose no SQL, settings or commands that are not in the data: you
may quote a finding's recommended_sql verbatim in a code block; otherwise describe
the problem and refer to the finding. risk_tier is pg_sage's risk class for that
recommendation; high-risk changes always need operator approval. Any other
statement is removed before the briefing is sent.

` + llm.UntrustedDataRule

	// Finding titles and object names are DB-influenced, so the
	// briefing data is delimited as untrusted data (G3-B07). A
	// JSON-wrapped prose answer is unwrapped before grounding (G3-B11).
	// The system prompt must not say "JSON": that turns on json_object mode.
	// An answer cut at the token limit is kept and marked as cut (llm-07).
	pctx := llm.WithPurpose(ctx, "briefing") // llm-04: usage ledger label
	resp, err := w.llm.ChatResult(pctx, system,
		llm.UntrustedData("briefing_data", structured), briefingMaxTokens)
	if err != nil {
		return "", resp.Tokens, err
	}
	text := llm.UnwrapText(resp.Content)
	if resp.Truncated {
		w.logFn("WARN", "briefing: LLM answer cut at the model's token limit "+
			"(finish_reason=%s, tokens=%d, cap=%d); stored with a truncation note",
			resp.FinishReason, resp.Tokens, briefingMaxTokens)
		text = withTruncationNote(text)
	}
	return text, resp.Tokens, nil
}
