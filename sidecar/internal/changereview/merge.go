package changereview

import (
	"github.com/pg-sage/sidecar/internal/agentloop"
)

// VerdictRank orders verdicts: safe 0, review 1, risky 2; unknown -1.
func VerdictRank(v string) int {
	switch v {
	case VerdictSafe:
		return 0
	case VerdictReview:
		return 1
	case VerdictRisky:
		return 2
	}
	return -1
}

// MaxVerdict is the stricter of a and b; an unknown verdict loses.
func MaxVerdict(a, b string) string {
	if VerdictRank(b) > VerdictRank(a) {
		return b
	}
	if VerdictRank(a) < 0 {
		return b
	}
	return a
}

// VerdictOf is the deterministic verdict of rule findings: risky if any is
// critical (or of an unknown severity: fail closed), review if any is a
// warning, otherwise safe. Text mode, or an unknown parse mode, is never
// safe.
func VerdictOf(findings []Finding, parseMode string) string {
	v := VerdictSafe
	for _, f := range findings {
		switch f.Severity {
		case SeverityInfo:
		case SeverityWarning:
			v = MaxVerdict(v, VerdictReview)
		default:
			return VerdictRisky
		}
	}
	if parseMode != ParseAST {
		v = MaxVerdict(v, VerdictReview)
	}
	return v
}

// Merged is a review's result after the merge rule.
type Merged struct {
	Verdict              string
	DeterministicVerdict string
	ModelStatus          string
	Statements           []Statement
	Findings             []Finding
	Evidence             []Evidence
	Dropped              []Dropped
	NotDetermined        []NotDetermined
	Intent               string
	Tokens               int
	Transcript           *agentloop.Transcript
}

// Merge applies the merge rule to a deterministic analysis and an optional
// model result (nil: no model). The deterministic findings come first,
// unchanged; model findings follow with severity clamped to warning,
// origin model and no rule id (CR-19 and CR-20 are the only rule ids a
// reasoner may set); model evidence never replaces deterministic evidence;
// a model finding citing evidence the review does not hold is dropped.
// Final verdict = max(deterministic, review when a model finding or a
// model-translated statement survives). A review with no SQL statement
// analyzed is at least review: nothing established that it is safe.
func Merge(a Analysis, r *Reasoning) Merged {
	m := Merged{DeterministicVerdict: VerdictOf(a.Findings, a.ParseMode),
		ModelStatus: ModelNoModel}
	if len(a.Statements) == 0 {
		m.DeterministicVerdict = MaxVerdict(m.DeterministicVerdict, VerdictReview)
	}
	m.Findings = append([]Finding(nil), a.Findings...)
	m.Statements = append([]Statement(nil), a.Statements...)
	m.Evidence = append([]Evidence(nil), a.Evidence...)
	m.NotDetermined = append([]NotDetermined(nil), a.NotDetermined...)
	m.Verdict = m.DeterministicVerdict
	if r == nil {
		return m
	}
	m.ModelStatus = modelStatusOf(r.ModelStatus)
	m.Intent, m.Tokens, m.Transcript = r.Intent, r.Tokens, r.Transcript
	m.Dropped = append(m.Dropped, r.Dropped...)
	m.NotDetermined = append(m.NotDetermined, r.NotDetermined...)
	m.Evidence = mergeEvidence(m.Evidence, r.Evidence)
	held := evidenceSet(m.Evidence)
	raised := false
	for _, f := range r.Findings {
		f, reason := clampModelFinding(f, held)
		if reason != "" {
			m.Dropped = append(m.Dropped, Dropped{Kind: "finding", Text: f.Title,
				Reason: reason})
			continue
		}
		m.Findings = append(m.Findings, f)
		raised = true
	}
	if translated := translatedStatements(r.Statements); len(translated) > 0 {
		m.Findings = append(m.Findings, cr19(translated))
		raised = true
	}
	m.Statements = append(m.Statements, r.Statements...)
	if raised {
		m.Verdict = MaxVerdict(m.Verdict, VerdictReview)
	}
	return m
}

func modelStatusOf(s string) string {
	switch s {
	case ModelOK, ModelNoModel, ModelBudget, ModelRateLimited, ModelTimeout, ModelError,
		ModelSkipped:
		return s
	}
	return ModelError
}

// mergeEvidence appends the model's evidence whose ids are new.
func mergeEvidence(det, model []Evidence) []Evidence {
	held := evidenceSet(det)
	for _, e := range model {
		if !held[e.ID] {
			det = append(det, e)
			held[e.ID] = true
		}
	}
	return det
}

func evidenceSet(ev []Evidence) map[string]bool {
	out := make(map[string]bool, len(ev))
	for _, e := range ev {
		out[e.ID] = true
	}
	return out
}

// clampModelFinding is f as a model finding the review may store: at most
// warning, origin model (rule origin only for CR-19 and CR-20), every
// cited evidence held. reason is why it cannot be stored ("" when it can).
func clampModelFinding(f Finding, held map[string]bool) (Finding, string) {
	f.EvidenceIDs = append([]string{}, f.EvidenceIDs...)
	rule := f.RuleID == RuleModelTranslated || f.RuleID == RuleNotAnalyzableSQL
	if len(f.EvidenceIDs) == 0 && !rule {
		return f, "uncited"
	}
	for _, id := range f.EvidenceIDs {
		if !held[id] {
			return f, "unknown_evidence"
		}
	}
	if f.Severity != SeverityInfo {
		f.Severity = SeverityWarning
	}
	if rule {
		f.Origin = OriginRule
	} else {
		f.RuleID, f.Origin = "", OriginModel
	}
	return f, ""
}

func translatedStatements(stmts []Statement) []Statement {
	var out []Statement
	for _, s := range stmts {
		if s.Origin == OriginModelTranslated {
			out = append(out, s)
		}
	}
	return out
}

// cr19 is the finding that model-translated DDL can never be safe.
func cr19(translated []Statement) Finding {
	f := Finding{RuleID: RuleModelTranslated, Kind: "model_translated_ddl",
		Severity: SeverityWarning, Origin: OriginRule,
		Title: "DDL translated by the model from ORM code",
		Text: "pg_sage reviewed SQL its model derived from the migration code; run the " +
			"tool's SQL export and review that SQL to confirm.",
		EvidenceIDs: []string{}}
	for _, s := range translated {
		if s.EvidenceID != "" {
			f.EvidenceIDs = append(f.EvidenceIDs, s.EvidenceID)
		}
	}
	return f
}
