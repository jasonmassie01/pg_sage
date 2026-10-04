package tuning

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Setting and storage-parameter changes are judged against their own
// history (lifeos, v1.10.0: work_mem ratcheted up 1MB a cycle on
// cumulative temp totals): no new proposal while the last change's
// verification is open, evidence from after the last change once it is
// decided, and an operator for the third same-direction change in a week.

const (
	// settingHistoryWindow is how far back changes count.
	settingHistoryWindow = 7 * 24 * time.Hour
	// ratchetChanges is the number of same-direction changes in the
	// window, this one included, that needs an operator.
	ratchetChanges = 3
)

// SettingAction is one executed change of a setting ("work_mem") or a
// table's storage parameter ("public.orders|fillfactor").
type SettingAction struct {
	ActionID   int64
	ExecutedAt time.Time
	Key        string
	From, To   string // From "" when it was reset to the default or unknown
	Verdict    string // "" or "pending" until the verification concludes
	DecidedAt  *time.Time
}

func (a SettingAction) pending() bool {
	return a.Verdict == "" || a.Verdict == verify.OutcomePending
}

// ledgerAction is one sage.action_log row with its outcome verdict.
type ledgerAction struct {
	ID         int64
	ExecutedAt time.Time
	SQL        string
	Rollback   string
	Outcome    string
	Verdict    string
	DecidedAt  *time.Time
}

// settingActionsFrom keeps the ledger rows that changed a setting or a
// storage parameter and did not fail, one entry per changed key.
func settingActionsFrom(rows []ledgerAction) []SettingAction {
	var out []SettingAction
	for _, r := range rows {
		if r.Outcome != "success" && r.Outcome != "pending" {
			continue
		}
		verdict := r.Verdict
		if r.Outcome == "pending" {
			verdict = verify.OutcomePending
		}
		base := SettingAction{ActionID: r.ID, ExecutedAt: r.ExecutedAt, Verdict: verdict,
			DecidedAt: r.DecidedAt}
		if st, ok := pgconf.ParseAlterSystem(r.SQL); ok && !st.Reset {
			base.Key, base.To = st.Name, st.Value
			if rb, ok := pgconf.ParseAlterSystem(r.Rollback); ok && !rb.Reset {
				base.From = rb.Value
			}
			out = append(out, base)
			continue
		}
		out = append(out, reloptionActions(base, r)...)
	}
	return out
}

func reloptionActions(base SettingAction, r ledgerAction) []SettingAction {
	st, ok := pgconf.ParseAlterTableReloptions(r.SQL)
	table := canonicalRef(st.Table)
	if !ok || st.Reset || table == "" {
		return nil
	}
	var out []SettingAction
	for _, o := range st.Options {
		a := base
		a.Key, a.To = table+"|"+strings.ToLower(o.Key), o.Value
		out = append(out, a)
	}
	return out
}

// historyCheck decides a change of key from current to value: a rejection,
// or the approval reason and history to put on the finding.
func (v *validator) historyCheck(p Proposal, key, current, value string,
	parse func(string) (float64, bool)) (*Judged, string, []map[string]any) {
	if v.historyErr != nil {
		j := reject(p, ReasonUnavailable, "the change history of %s is unreadable, so "+
			"repeated changes cannot be bounded: %v", key, v.historyErr)
		return &j, "", nil
	}
	acts := v.history[key]
	if n := len(acts); n > 0 {
		last := acts[n-1]
		if last.pending() {
			j := reject(p, ReasonVerificationPending, "action #%d set %s to %s at %s and "+
				"its verification has not concluded", last.ActionID, key, last.To,
				last.ExecutedAt.UTC().Format(time.RFC3339))
			return &j, "", nil
		}
		if v.window.from.IsZero() || v.window.from.Before(last.ExecutedAt) {
			j := reject(p, ReasonNoRecentEvidence, "action #%d changed %s at %s (%s); a "+
				"further change needs evidence measured after it", last.ActionID, key,
				last.ExecutedAt.UTC().Format(time.RFC3339), last.Verdict)
			return &j, "", nil
		}
	}
	approval, hist := ratchet(key, current, value, acts, parse)
	return nil, approval, hist
}

// ratchet is the approval reason when this change would be the third in
// the same direction within the window, with the history for the card.
func ratchet(key, current, value string, acts []SettingAction,
	parse func(string) (float64, bool)) (string, []map[string]any) {
	dir := direction(current, value, parse)
	if dir == 0 {
		return "", nil
	}
	var same []SettingAction
	for _, a := range acts {
		if direction(a.From, a.To, parse) == dir {
			same = append(same, a)
		}
	}
	if len(same)+1 < ratchetChanges {
		return "", nil
	}
	var steps []string
	var hist []map[string]any
	for _, a := range same {
		steps = append(steps, fmt.Sprintf("%s -> %s (#%d, %s)", a.From, a.To, a.ActionID,
			a.Verdict))
		hist = append(hist, map[string]any{"action_id": a.ActionID, "from": a.From,
			"to": a.To, "verdict": a.Verdict,
			"executed_at": a.ExecutedAt.UTC().Format(time.RFC3339)})
	}
	return fmt.Sprintf("%s would change in the same direction %d times in 7 days "+
		"(%s, then %s -> %s); a repeated change needs an operator", key, len(same)+1,
		strings.Join(steps, ", "), current, value), hist
}

// direction is +1 when to is above from, -1 below, 0 when unknown.
func direction(from, to string, parse func(string) (float64, bool)) int {
	a, okA := parse(from)
	b, okB := parse(to)
	switch {
	case !okA || !okB || a == b:
		return 0
	case b > a:
		return 1
	}
	return -1
}

// gucParser parses a setting's values into comparable numbers.
func gucParser(name string) func(string) (float64, bool) {
	return func(s string) (float64, bool) {
		if strings.TrimSpace(s) == "" {
			return 0, false
		}
		if doc, ok := pgconf.Docs[name]; ok {
			n, err := pgconf.ParseValue(s, doc)
			return n, err == nil
		}
		return plainNumber(s)
	}
}

func plainNumber(s string) (float64, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(pgconf.Unquote(s)), 64)
	return n, err == nil
}

// applyHistory puts the evidence window and, for a ratchet, the approval
// reason and history on an admitted finding.
func (v *validator) applyHistory(j Judged, approval string, hist []map[string]any) Judged {
	if j.Finding == nil {
		return j
	}
	f := *j.Finding
	f.Detail = cloneDetail(f.Detail)
	f.Detail["evidence_window"] = map[string]any{
		"from": v.window.from.UTC().Format(time.RFC3339),
		"to":   v.window.to.UTC().Format(time.RFC3339)}
	if approval != "" {
		if prior, _ := f.Detail[analyzer.DetailApprovalRequired].(string); prior != "" {
			approval = prior + "; " + approval
		}
		f.Detail[analyzer.DetailApprovalRequired] = approval
		f.Detail["setting_history"] = hist
	}
	j.Finding = &f
	return j
}

func cloneDetail(d map[string]any) map[string]any {
	out := make(map[string]any, len(d)+3)
	for k, v := range d {
		out[k] = v
	}
	return out
}

// prepare reads what the cycle's judgments need: the interval the evidence
// covers, the recent setting changes and the in-flight indexes (queued, or
// the open proposals).
func (v *validator) prepare(ctx context.Context, prev *collector.Snapshot,
	open []analyzer.Finding) {
	v.loadInFlight(ctx, open)
	if priorCounters(v.cur, prev) != nil {
		v.window = evidenceWindow{from: prev.CollectedAt, to: v.cur.CollectedAt}
	}
	acts, err := v.a.deps.Store.SettingActions(ctx, v.a.now().Add(-settingHistoryWindow))
	if err != nil {
		v.historyErr = err
		return
	}
	sort.SliceStable(acts, func(i, j int) bool {
		return acts[i].ExecutedAt.Before(acts[j].ExecutedAt)
	})
	v.history = map[string][]SettingAction{}
	for _, a := range acts {
		v.history[a.Key] = append(v.history[a.Key], a)
	}
}

// evidenceWindow is the interval the case's counters cover; zero without
// an earlier sample.
type evidenceWindow struct{ from, to time.Time }
