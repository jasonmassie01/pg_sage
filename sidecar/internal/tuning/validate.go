package tuning

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/tuner"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Proposal validation: every proposal is judged deterministically before
// it becomes a finding. pg_sage generates the SQL from the typed fields;
// the optimizer's validator, rejection memory and what-if gate, the
// configuration allowlists and ranges, the tuner's hint checks and the
// confirmed facts all apply. Validation only narrows: the policy gate and
// trust level still decide what runs.

// Producer, PredictionSource and the agent's own finding categories.
const (
	Producer           = "tuning_agent"
	PredictionSource   = "tuning_agent"
	CategoryIndexDrop  = "tuning_index_drop"
	CategoryStatistics = "query_create_statistics"
)

// Verdict is what validation decided.
type Verdict string

// Verdicts.
const (
	VerdictAdmitted   Verdict = "admitted"
	VerdictRedirected Verdict = "redirected"
	VerdictRejected   Verdict = "rejected"
)

// Reason is why a proposal was rejected.
type Reason string

// Rejection reasons.
const (
	ReasonUnsupported      Reason = "unsupported_form"
	ReasonDisabled         Reason = "disabled"
	ReasonUncited          Reason = "uncited"
	ReasonUnknownEvidence  Reason = "unknown_evidence"
	ReasonNoPrediction     Reason = "no_prediction"
	ReasonOutOfCase        Reason = "out_of_case"
	ReasonInvalid          Reason = "invalid"
	ReasonWhatIfRejected   Reason = "what_if_rejected"
	ReasonAlreadyMeasured  Reason = "already_measured"
	ReasonOperatorRejected Reason = "operator_rejected"
	ReasonDuplicate        Reason = "duplicate"
	ReasonNotWorkload      Reason = "not_workload"
	ReasonUnavailable      Reason = "unavailable"
	ReasonOutOfScope       Reason = "out_of_scope"
)

// Judged is the decision on one proposal.
type Judged struct {
	Proposal   Proposal
	Verdict    Verdict
	Reason     Reason
	Detail     string
	Finding    *analyzer.Finding
	Prediction verify.Prediction
	Class      string
	Tables     []string
	Confidence Confidence
	Case       Case
	// Hint is the checked hint the tuner records if the proposal survives
	// the cycle's cap (query hints only).
	Hint *tuner.HintProposal
}

// validator judges the proposals of one cycle.
type validator struct {
	a         *Agent
	cur       *collector.Snapshot
	w         Workload
	confirmed []facts.Fact
	rejected  map[string]bool
	dropping  map[string]bool
}

func (a *Agent) newValidator(cur *collector.Snapshot, w Workload, confirmed []facts.Fact,
	rejected map[string]bool) *validator {
	return &validator{a: a, cur: cur, w: w, confirmed: confirmed, rejected: rejected,
		dropping: map[string]bool{}}
}

func reject(p Proposal, r Reason, format string, args ...any) Judged {
	return Judged{Proposal: p, Verdict: VerdictRejected, Reason: r,
		Detail: fmt.Sprintf(format, args...)}
}

// judge decides one proposal of case c.
func (v *validator) judge(ctx context.Context, c Case, ev evidenceSet, p Proposal) Judged {
	if j, done := v.precheck(c, ev, p); done {
		return j
	}
	var j Judged
	switch p.Type {
	case ProposeIndexCreate:
		j = v.judgeIndexCreate(ctx, c, p)
	case ProposeIndexDrop:
		j = v.judgeIndexDrop(c, p)
	case ProposeGUC:
		j = v.judgeGUC(c, p)
	case ProposeReloption:
		j = v.judgeReloption(c, p)
	case ProposeStatistics:
		j = v.judgeStatistics(ctx, c, p)
	case ProposeQueryHint:
		j = v.judgeHint(ctx, c, p)
	}
	j.Case = c
	if j.Verdict == VerdictRejected || j.Finding == nil {
		return j
	}
	return v.finish(c, ev, j)
}

// precheck applies the rules every type shares: a known, allowed type,
// known citations and a prediction.
func (v *validator) precheck(c Case, ev evidenceSet, p Proposal) (Judged, bool) {
	if !slices.Contains(proposalTypes, p.Type) {
		return reject(p, ReasonUnsupported, "%q is not a proposal type", p.Type), true
	}
	if !v.a.allowed(p.Type) {
		return reject(p, ReasonDisabled, "%s proposals are switched off or unavailable",
			p.Type), true
	}
	if len(p.Evidence) == 0 {
		return reject(p, ReasonUncited, "the proposal cites no evidence"), true
	}
	var unknown []string
	for _, id := range p.Evidence {
		if _, ok := ev[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return reject(p, ReasonUnknownEvidence, "unknown evidence %s",
			strings.Join(unknown, ", ")), true
	}
	if p.Type != ProposeIndexDrop && p.ExpectedChangePct == nil {
		return reject(p, ReasonNoPrediction, "the proposal predicts no effect"), true
	}
	return Judged{}, false
}

// tableInCase resolves a proposal's table and checks it belongs to the
// case and is workload.
func (v *validator) tableInCase(c Case, p Proposal, ref string) (string, Judged, bool) {
	name := canonicalRef(ref)
	if name == "" {
		return "", reject(p, ReasonInvalid, "%q is not a schema-qualified table", ref), false
	}
	if !slices.Contains(c.Tables, name) {
		return "", reject(p, ReasonOutOfCase, "%s is not a table of case %s", name, c.ID),
			false
	}
	if info := v.w.Tables[name]; info.Class == ClassTest || info.Class == ClassSage {
		return "", reject(p, ReasonNotWorkload, "%s is not workload: %s", name,
			info.Reason), false
	}
	return name, Judged{}, true
}

// targets are the proposal's target statements that are workload
// statements of the snapshot, else the case's statements.
func (v *validator) targets(c Case, p Proposal) []int64 {
	var out []int64
	for _, q := range p.TargetQueryIDs {
		if v.w.IsWorkload(int64(q)) && !slices.Contains(out, int64(q)) {
			out = append(out, int64(q))
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, s := range c.Statements {
		out = append(out, s.QueryID)
	}
	return out
}

// normalizeSQL is SQL as compared with an operator's rejections: no
// trailing semicolon, whitespace collapsed, lower case.
func normalizeSQL(sql string) string {
	s := strings.TrimSpace(sql)
	s = strings.TrimSpace(strings.TrimRight(s, "; \t\n"))
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// predictedChange checks a predicted change for a metric that should
// fall (fall true) or rise: no predicted regression, no impossible fall.
func predictedChange(p Proposal, fall bool) (float64, string) {
	pct := *p.ExpectedChangePct
	switch {
	case fall && pct > 0:
		return 0, fmt.Sprintf("the proposal predicts the metric rises %.1f%%", pct)
	case !fall && pct < 0:
		return 0, fmt.Sprintf("the proposal predicts the metric falls %.1f%%", -pct)
	case pct < -100:
		return 0, fmt.Sprintf("a fall of %.1f%% is impossible", -pct)
	}
	return pct, ""
}
