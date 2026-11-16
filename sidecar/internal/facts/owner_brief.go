package facts

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The owner brief (evid-03a): what a senior DBA knows and telemetry lacks
// (who owns the database, its peak windows, growth and retention), kept as
// one fact per database (subject kind database, subject "*"). It never
// binds: Bind skips it and the gate allows exactly what it allowed without
// it. Prompts carry it first, as context.

// briefKeys are the brief's value keys in the order Describe says them,
// with the words it uses.
var briefKeys = []struct{ key, word string }{{"owner", "owner"},
	{"contact", "contact"}, {"peak_windows", "peak"}, {"growth", "growth"},
	{"growth_pct_month", "growth"}, {"retention", "retention"}, {"note", "note"}}

// maxGrowthPctMonth bounds growth_pct_month (percent per month).
const maxGrowthPctMonth = 1000

// briefGuidance says what the brief means for a proposal; briefCutNote
// ends a brief line the prompt bound cut.
const (
	briefGuidance = "It is context, not a rule: weigh it when you time or size a " +
		"proposal; it never allows or forbids a change."
	briefCutNote = " (rest of the brief omitted)"
)

// validateBrief checks a brief's value (keys and lengths are checked by
// validateValue): blank keys are dropped, at least one key remains,
// growth_pct_month is a number in 0-1000 and peak_windows is one or more
// windows of the policy grammar separated by ';'.
func validateBrief(value map[string]string) (map[string]string, error) {
	for k, v := range value {
		if v == "" {
			delete(value, k)
		}
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("%w: an owner brief says at least one of owner, contact, "+
			"peak_windows, growth, growth_pct_month, retention or note", ErrInvalidValue)
	}
	if g, ok := value["growth_pct_month"]; ok {
		pct, err := strconv.ParseFloat(g, 64)
		if err != nil || math.IsNaN(pct) || pct < 0 || pct > maxGrowthPctMonth {
			return nil, fmt.Errorf("%w: growth_pct_month %q is not a number from 0 to %d",
				ErrInvalidValue, g, maxGrowthPctMonth)
		}
	}
	if w, ok := value["peak_windows"]; ok {
		for _, part := range strings.Split(w, ";") {
			if _, err := policy.ParseWindow(strings.TrimSpace(part)); err != nil {
				return nil, fmt.Errorf("%w: peak_windows: %v", ErrInvalidValue, err)
			}
		}
	}
	return value, nil
}

// describeBrief is the brief in words: "owner brief: owner X, peak <window>,
// ...", in briefKeys order.
func describeBrief(value map[string]string) string {
	parts := make([]string, 0, len(briefKeys))
	for _, k := range briefKeys {
		v := value[k.key]
		if v == "" {
			continue
		}
		if k.key == "growth_pct_month" {
			v += "%/month"
		}
		parts = append(parts, k.word+" "+v)
	}
	return "owner brief: " + strings.Join(parts, ", ")
}

// briefPromptLine is the brief's prompt line, at most maxPromptLine bytes:
// a long brief is cut before its guidance, and the line says so.
func briefPromptLine(f Fact) string {
	head := fmt.Sprintf("- fact #%d (operator-confirmed): ", f.ID)
	desc := cleanText(f.Describe())
	tail := ". " + briefGuidance
	if len(head)+len(desc)+len(tail) <= maxPromptLine {
		return head + desc + tail
	}
	room := maxPromptLine - len(head) - len(tail) - len(briefCutNote)
	return head + clip(desc, max(room, 0)) + tail + briefCutNote
}

// Brief states of a BriefMeasure.
const (
	BriefPresent = "present"
	BriefAbsent  = "absent"
)

// StepCounts are the accepted (approved or executed) and rejected action
// queue rows decided in [From, To).
type StepCounts struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Accepted int64     `json:"accepted"`
	Rejected int64     `json:"rejected"`
}

// BriefMeasure compares the steps decided since the split with those of
// the same length before it. The split is the confirmed brief's latest
// decision, or the caller's since when there is no confirmed brief.
type BriefMeasure struct {
	Brief  string     `json:"brief"`
	FactID int64      `json:"fact_id,omitempty"`
	Before StepCounts `json:"before"`
	After  StepCounts `json:"after"`
}

const briefOutcomeSQL = `/* pg_sage facts */
WITH brief AS (
    SELECT id, decided_at FROM sage.facts
     WHERE fact_type = 'owner_brief' AND subject_kind = 'database' AND subject = '*'
       AND status = 'confirmed' AND decided_at IS NOT NULL
       AND (expires_at IS NULL OR expires_at > now())),
win AS (
    SELECT s.split, now() AS until, s.split - (now() - s.split) AS start
      FROM (SELECT LEAST(COALESCE((SELECT decided_at FROM brief), $1::timestamptz),
                         now()) AS split) s),
steps AS (
    SELECT q.status, COALESCE(q.decided_at, q.proposed_at) AS at
      FROM sage.action_queue q, win
     WHERE COALESCE(q.decided_at, q.proposed_at) >= win.start
       AND q.status IN ('approved', 'executed', 'rejected'))
SELECT COALESCE((SELECT id FROM brief), 0), win.start, win.split, win.until,
       count(*) FILTER (WHERE s.at < win.split AND s.status <> 'rejected'),
       count(*) FILTER (WHERE s.at < win.split AND s.status = 'rejected'),
       count(*) FILTER (WHERE s.at >= win.split AND s.status <> 'rejected'),
       count(*) FILTER (WHERE s.at >= win.split AND s.status = 'rejected')
  FROM win LEFT JOIN steps s ON true
 GROUP BY win.start, win.split, win.until`

// BriefOutcome measures whether the accepted-step rate moves with a
// brief: the approved or executed versus rejected sage.action_queue rows
// since the brief was confirmed and in the same length of time before
// (split at since when the database has no confirmed brief). The read is
// read-only and bounded (statement and lock timeouts).
func BriefOutcome(ctx context.Context, db txBeginner, since time.Time) (BriefMeasure,
	error) {
	if since.IsZero() {
		return BriefMeasure{}, fmt.Errorf("%w: owner brief outcome needs a since time",
			ErrInvalidValue)
	}
	var m BriefMeasure
	err := boundedRead(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, briefOutcomeSQL, since).Scan(&m.FactID, &m.Before.From,
			&m.Before.To, &m.After.To, &m.Before.Accepted, &m.Before.Rejected,
			&m.After.Accepted, &m.After.Rejected)
	})
	if err != nil {
		return BriefMeasure{}, fmt.Errorf("owner brief outcome: %w", err)
	}
	m.After.From = m.Before.To
	m.Brief = BriefAbsent
	if m.FactID != 0 {
		m.Brief = BriefPresent
	}
	return m, nil
}
