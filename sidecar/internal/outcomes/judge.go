package outcomes

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/verify"
)

// labels are the dashboard's action labels (actionLabel in
// web/src/pages/actions/workflow.js; a test keeps the two equal).
var labels = map[string]string{
	"analyze":                   "Update query-planning statistics",
	"analyze_table":             "Update query-planning statistics",
	"vacuum":                    "Clean up expired table rows",
	"vacuum_table":              "Clean up expired table rows",
	"create_index":              "Add an index",
	"create_index_concurrently": "Add an index",
	"drop_index":                "Remove an index",
	"drop_unused_index":         "Remove an unused index",
	"reindex":                   "Rebuild an index",
	"reindex_concurrently":      "Rebuild an index",
}

// LabelledActionTypes lists the action types Label words itself, sorted.
func LabelledActionTypes() []string {
	out := make([]string, 0, len(labels))
	for t := range labels {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Label is the action type in the dashboard's words.
func Label(actionType string) string {
	if l, ok := labels[actionType]; ok {
		return l
	}
	if actionType == "" {
		return "Change details unavailable"
	}
	text := strings.ReplaceAll(actionType, "_", " ")
	return strings.ToUpper(text[:1]) + text[1:]
}

// verdictOf is a row's verdict: the verifier's decided verdict (lab-01's
// record) in the plan vocabulary, else the plan step's verdict, else
// expired when the action's verification expired, else pending.
func verdictOf(outcomeVerdict, stepVerdict, actionOutcome string) string {
	if verify.DecidedVerdict(outcomeVerdict) {
		return string(changeplan.MapVerifierOutcome(outcomeVerdict))
	}
	if stepVerdict != "" {
		return stepVerdict
	}
	if actionOutcome == "verification_expired" {
		return VerdictExpired
	}
	return VerdictPending
}

// rollbackStateOf is whether a change can still be rolled back.
func rollbackStateOf(actionOutcome, rollbackSQL string) string {
	switch actionOutcome {
	case "rolled_back", "reverted":
		return RollbackRolledBack
	case "rollback_failed":
		return RollbackFailed
	case "rolling_back":
		return RollbackInProgress
	}
	if strings.TrimSpace(rollbackSQL) == "" {
		return RollbackNotReversible
	}
	return RollbackAvailable
}

// measuredVerdict reports a verdict judged on a measurement.
func measuredVerdict(v string) bool {
	return v == VerdictImproved || v == VerdictNoChange || v == VerdictRegressed
}

// measuredOf is the verifier's measured effect for verdict, or nil and
// why there is none.
func measuredOf(verdict string, observed []byte, reason string) (*Effect, string) {
	if !measuredVerdict(verdict) {
		return nil, unmeasuredReason(verdict, reason)
	}
	var o verify.Observed
	if len(observed) == 0 || json.Unmarshal(observed, &o) != nil ||
		(o.ChangePct == nil && o.Before == 0 && o.After == 0) {
		return nil, "no measurement was recorded"
	}
	if o.ChangePct != nil {
		return &Effect{Metric: o.Metric, ChangePct: round(*o.ChangePct, 1)}, ""
	}
	if o.Before == 0 {
		return nil, "no baseline to compare with (the metric was 0 before the change)"
	}
	return &Effect{Metric: o.Metric, ChangePct: round((o.After-o.Before)/o.Before*100, 1)}, ""
}

func unmeasuredReason(verdict, reason string) string {
	if reason != "" {
		return reason
	}
	switch verdict {
	case VerdictPending:
		return "not judged yet"
	case VerdictExpired:
		return "the verification deadline passed before a verdict"
	case VerdictUnverifiable:
		return "this target cannot be measured"
	}
	return "too little evidence to measure the effect"
}

// stepExpect is a plan step's expected effect.
type stepExpect struct {
	pct                       *float64
	direction, metric, source string
}

// expectedOf is the predicted effect stored before the change ran (the
// approval card's prediction), else the plan step's expectation.
func expectedOf(predicted []byte, step stepExpect) *Effect {
	var p verify.Prediction
	if len(predicted) > 0 && json.Unmarshal(predicted, &p) == nil && p.Predicts() {
		return &Effect{Metric: p.Metric, ChangePct: round(*p.ExpectedChangePct, 1),
			Source: p.Method}
	}
	if step.pct == nil || (step.direction != "down" && step.direction != "up") {
		return nil
	}
	pct := *step.pct
	if pct < 0 {
		pct = -pct
	}
	if step.direction == "down" {
		pct = -pct
	}
	return &Effect{Metric: step.metric, ChangePct: round(pct, 1), Source: step.source}
}

// tierOf is the highest verification-ladder tier (lab-03a) the change
// passed before production, from its stored prediction; "" for none or
// an unreadable prediction.
func tierOf(predicted []byte) string {
	var p verify.Prediction
	if len(predicted) == 0 || json.Unmarshal(predicted, &p) != nil {
		return ""
	}
	return verify.HighestPassedName(p.Ladder)
}
