package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Approval history (ux-07a): the approvals a person makes are evidence.
// Per database and self-initiated class, over ApprovalHistoryWindow, it
// counts the person-approved (L2) executions whose verification verdict
// is improved or neutral, since the last rollback, regression or
// rejection, and never an execution that was itself rolled back. It is
// counted in code from the outcomes the reconciler already recorded from
// sage.action_log and sage.action_outcome. Like the trust ramp it can
// create a proposal, never a grant: a person approves the promotion.

const (
	// ApprovalHistoryWindow is how far back approvals count.
	ApprovalHistoryWindow = 30 * 24 * time.Hour
	// DefaultAskAfterApprovals is trust.ask_after_approvals' default.
	DefaultAskAfterApprovals = 10
	// AskSnooze is how long "Not now" (a rejected ask) keeps the ask away.
	AskSnooze = 30 * 24 * time.Hour
	// EvidenceKindApprovalHistory marks a proposal that rests on the
	// approval history rather than the class record's bar.
	EvidenceKindApprovalHistory = "approval_history"
	// maxApprovalMarks bounds one pair's read (newest first); a longer
	// window only undercounts, never overcounts.
	maxApprovalMarks = 2000
)

// ApprovalHistory is one pair's approval record in the window. Approvals
// are counted since LastResetAt (the newest rollback, regression or
// rejection in the window); Qualifies is Approvals >= Required, never
// with Required 0 (the ask is off).
type ApprovalHistory struct {
	Approvals   int        `json:"approvals"`
	Improved    int        `json:"improved"`
	Neutral     int        `json:"neutral"`
	RolledBack  int        `json:"rolled_back"`
	Regressed   int        `json:"regressed"`
	Rejected    int        `json:"rejected"`
	Required    int        `json:"required"`
	Since       time.Time  `json:"since"`
	LastResetAt *time.Time `json:"last_reset_at,omitempty"`
	LastReset   string     `json:"last_reset,omitempty"`
	Qualifies   bool       `json:"qualifies"`
}

// approvalMark is one recorded outcome of a pair.
type approvalMark struct {
	actionLogID     int64
	level           Level
	source, verdict string
	result          string
	at              time.Time
}

// reset names the demerit a mark is, "" when it is none.
func (m approvalMark) reset() string {
	switch {
	case m.source == SourceRollback || m.verdict == CauseRolledBack:
		return CauseRolledBack
	case m.verdict == CauseRegressed || m.result == ResultHarmful ||
		m.result == ResultSafetyViolation:
		return CauseRegressed
	case m.verdict == CauseRejected || m.result == ResultRejected:
		return CauseRejected
	}
	return ""
}

// approval reports a person-approved execution that succeeded or held.
func (m approvalMark) approval() bool {
	return m.level == L2 && m.source == SourceExecutor &&
		(m.verdict == "improved" || m.verdict == "neutral")
}

// approvalMarksOf are c's marks as approval evidence. A neutral index
// create is reverted for no gain (Phase 1.3, verify's no_gain) and that
// revert is no rollback demerit (rollbackIsDemerit), so its neutral
// verdict is an approval that did not hold: it never counts.
func approvalMarksOf(c ActionClass, marks []approvalMark) []approvalMark {
	if c != ClassIndexCreate {
		return marks
	}
	out := make([]approvalMark, 0, len(marks))
	for _, m := range marks {
		if m.verdict != "neutral" {
			out = append(out, m)
		}
	}
	return out
}

// countApprovals is the approval history of marks (any order) since
// since, against required approvals.
func countApprovals(marks []approvalMark, since time.Time, required int) ApprovalHistory {
	h := ApprovalHistory{Required: max(required, 0), Since: since.UTC()}
	rolledBack := map[int64]bool{}
	for _, m := range marks {
		if m.at.Before(since) {
			continue
		}
		cause := m.reset()
		switch cause {
		case CauseRolledBack:
			h.RolledBack++
			rolledBack[m.actionLogID] = true
		case CauseRegressed:
			h.Regressed++
		case CauseRejected:
			h.Rejected++
		}
		if cause != "" && (h.LastResetAt == nil || m.at.After(*h.LastResetAt)) {
			at := m.at.UTC()
			h.LastResetAt, h.LastReset = &at, cause
		}
	}
	for _, m := range marks {
		if m.at.Before(since) || !m.approval() || (m.actionLogID != 0 &&
			rolledBack[m.actionLogID]) || (h.LastResetAt != nil && !m.at.After(*h.LastResetAt)) {
			continue
		}
		h.Approvals++
		if m.verdict == "improved" {
			h.Improved++
		} else {
			h.Neutral++
		}
	}
	h.Qualifies = h.Required > 0 && h.Approvals >= h.Required
	return h
}

const approvalMarksSQL = `SELECT /* pg_sage */ p.family, p.action_class, m.action_log_id,
	m.level, m.source, m.verdict, m.result, m.at
	FROM unnest($3::text[], $4::text[]) AS p(family, action_class)
	CROSS JOIN LATERAL (
		SELECT COALESCE(o.action_log_id, 0) AS action_log_id, o.level, o.source,
		       COALESCE(o.verdict, '') AS verdict, o.result,
		       COALESCE(o.observed_at, o.recorded_at) AS at
		  FROM sage.sre_autonomy_outcomes o
		 WHERE o.deployment_id = $1 AND o.database_name = $2 AND o.family = p.family
		   AND o.action_class = p.action_class
		   AND COALESCE(o.observed_at, o.recorded_at) >= $5
		 ORDER BY COALESCE(o.observed_at, o.recorded_at) DESC, o.id DESC
		 LIMIT $6) m`

// readApprovalMarks reads each pair's outcomes since since, newest first.
func (s *PostgresStore) readApprovalMarks(ctx context.Context, r reads, pairs []pairKey,
	since time.Time, out map[pairKey][]approvalMark) error {
	if len(pairs) == 0 {
		return nil
	}
	families, classes := pairArrays(pairs)
	return r.each(ctx, "read approval history", approvalMarksSQL,
		[]any{s.deployment, s.database, families, classes, since, maxApprovalMarks},
		func(rows pgx.Rows) error {
			var f, c string
			var m approvalMark
			var level int16
			if err := rows.Scan(&f, &c, &m.actionLogID, &level, &m.source, &m.verdict,
				&m.result, &m.at); err != nil {
				return err
			}
			m.level, m.at = Level(level), m.at.UTC()
			key := pairKey{Family(f), ActionClass(c)}
			out[key] = append(out[key], m)
			return nil
		})
}

// selfPairs are the pairs of self-initiated families.
func selfPairs(pairs []pairKey) []pairKey {
	var out []pairKey
	for _, p := range pairs {
		if IsSelfInitiated(p.family) {
			out = append(out, p)
		}
	}
	return out
}

// AssessApprovalHistory holds a self-initiated pair's approval history
// against target: only the L2 to L3 step, a reversible class whose cap
// allows L3, the L3 observation floor passed and enough approvals.
func AssessApprovalHistory(target Level, ev Evidence) Assessment {
	spec, _ := Spec(ev.Class)
	checks := []Check{
		{Name: "approval_history_step", Met: target == L3, Observed: target.String(),
			Required: "the L2 to L3 step"},
		{Name: "self_initiated", Met: IsSelfInitiated(ev.Family), Observed: string(ev.Family),
			Required: "a class pg_sage runs on its own initiative"},
		{Name: "reversible", Met: spec.Reversibility == Reversible,
			Observed: string(spec.Reversibility), Required: "a class that can be undone"},
		capCheck(L3, ev), floorCheck(L3, ev), approvalHistoryCheck(ev),
	}
	if !checks[2].Met {
		checks[2].How = fmt.Sprintf("%s cannot be undone: it always asks first.", ev.Class)
	}
	met := true
	for _, c := range checks {
		met = met && c.Met
	}
	return Assessment{Target: target, Met: met, Checks: checks}
}

func approvalHistoryCheck(ev Evidence) Check {
	c := Check{Name: "approval_history", Observed: "not read",
		Required: "person approvals with no rollback in 30 days"}
	h := ev.ApprovalHistory
	if h == nil {
		return c
	}
	c.Observed = fmt.Sprint(h.Approvals)
	if h.Required <= 0 {
		c.Required = "trust.ask_after_approvals above 0 (it is off)"
		return c
	}
	c.Met = h.Qualifies
	c.Required = fmt.Sprintf(">= %d person approvals with no rollback, regression or "+
		"rejection in 30 days", h.Required)
	if !c.Met {
		c.How = fmt.Sprintf("%d more person approvals of %s, each verified improved or "+
			"neutral, with no rollback (%d so far).", h.Required-h.Approvals, ev.Class,
			h.Approvals)
	}
	return c
}

// ApprovalAsk is the inbox's question for a proposal whose evidence holds
// a qualifying approval history; false for any other proposal.
func ApprovalAsk(p Proposal, database string) (string, bool) {
	var body struct {
		Evidence struct {
			History *ApprovalHistory `json:"approval_history"`
		} `json:"evidence"`
	}
	if p.To != L3 || len(p.Evidence) == 0 || json.Unmarshal(p.Evidence, &body) != nil {
		return "", false
	}
	h := body.Evidence.History
	if h == nil || !h.Qualifies || h.Approvals <= 0 {
		return "", false
	}
	return fmt.Sprintf("You approved %s %d times on %s with no rollback; allow it "+
		"unattended?", p.Class, h.Approvals, database), true
}

// proposalKind is the evidence kind a proposal rests on ("" the bar).
func proposalKind(p Proposal) string {
	var body struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(p.Evidence, &body) != nil {
		return ""
	}
	return body.Kind
}

const askSnoozesSQL = `SELECT /* pg_sage */ DISTINCT family, action_class
	FROM sage.sre_autonomy_proposals
	WHERE deployment_id = $1 AND database_name = $2 AND status = 'rejected'
	  AND evidence->>'kind' = 'approval_history' AND decided_at >= $3`

// readAskSnoozes marks the pairs whose approval ask a person put off
// ("Not now": rejected) since since.
func (s *PostgresStore) readAskSnoozes(ctx context.Context, r reads, since time.Time,
	out map[pairKey]bool) error {
	return r.each(ctx, "read snoozed approval asks", askSnoozesSQL,
		[]any{s.deployment, s.database, since}, func(rows pgx.Rows) error {
			var f, c string
			if err := rows.Scan(&f, &c); err != nil {
				return err
			}
			out[pairKey{Family(f), ActionClass(c)}] = true
			return nil
		})
}
