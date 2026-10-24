package reasoner

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Deterministic post-processing of the model's answer: what of it the
// review keeps, and why the rest was dropped.

// Intents the model may judge; anything else is IntentUnclear.
const IntentUnclear = "unclear"

var intents = map[string]bool{"expand": true, "contract": true, "backfill": true,
	"expand_contract": true, "other": true, IntentUnclear: true}

// ModelNotDeterminedReason marks what the model said it could not decide.
const ModelNotDeterminedReason = "the model could not determine this"

var (
	kindArg     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	languageArg = regexp.MustCompile(`^[a-z0-9+#._-]{1,20}$`)
	ormArg      = regexp.MustCompile(`^[a-z0-9_.-]{1,40}$`)
)

type finalAnswer struct {
	Statements    []finalStatement `json:"statements"`
	Findings      []finalFinding   `json:"findings"`
	Intent        string           `json:"intent"`
	NotDetermined []string         `json:"not_determined"`
}

type finalStatement struct {
	EvidenceID string  `json:"evidence_id"`
	File       string  `json:"file"`
	Line       int     `json:"line"`
	ORM        string  `json:"orm"`
	Confidence float64 `json:"confidence"`
	InLoop     bool    `json:"in_loop"`
}

type finalFinding struct {
	Severity    string   `json:"severity"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
	Suggestion  *struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	} `json:"suggestion"`
}

// conclude is the review's reasoning from a finished run.
func (s *session) conclude(res agentloop.Result) changereview.Reasoning {
	out := s.base(res)
	if res.Final != nil {
		var fin finalAnswer
		if err := json.Unmarshal(res.Final, &fin); err != nil {
			out.NotDetermined = append(out.NotDetermined, changereview.NotDetermined{
				Subject: "the model's review", Reason: "its answer did not decode"})
		}
		s.statements(&out, fin.Statements, res.Evidence)
		s.findings(&out, fin.Findings, res.Evidence)
		s.ruleHits(&out)
		out.Intent = intentText(fin.Intent, res.Claims)
		for _, d := range res.Dropped {
			out.Dropped = append(out.Dropped, changereview.Dropped{Kind: "claim",
				Text: d.Text, Reason: d.Reason})
		}
		out.NotDetermined = append(out.NotDetermined, modelNotDetermined(fin.NotDetermined)...)
	}
	s.finish(&out)
	return out
}

// base is what every run reports: status, tokens, the redacted transcript
// and the files cut from the offer.
func (s *session) base(res agentloop.Result) changereview.Reasoning {
	out := changereview.Reasoning{ModelStatus: modelStatus(res.Transcript.Stop),
		Tokens: res.Transcript.Tokens, Transcript: redactTranscript(res.Transcript)}
	for _, p := range s.offered.Cut {
		out.NotDetermined = append(out.NotDetermined, changereview.NotDetermined{Subject: p,
			Reason: fmt.Sprintf("not offered to the model: over max_offered_diff_bytes (%d)",
				s.in.Config.MaxOfferedDiffBytes)})
	}
	return out
}

// failed is the reasoning of a run that ended with an error (the budget
// store was unavailable): model status error.
func (s *session) failed(res agentloop.Result, err error) changereview.Reasoning {
	out := s.base(res)
	out.ModelStatus = changereview.ModelError
	out.NotDetermined = append(out.NotDetermined, changereview.NotDetermined{
		Subject: "model stage", Reason: "stopped: " + clipRunes(err.Error(), 200)})
	s.finish(&out)
	return out
}

// offline is the reasoning without a model run.
func (s *session) offline(status string) changereview.Reasoning {
	out := changereview.Reasoning{ModelStatus: status}
	s.finish(&out)
	return out
}

// finish adds CR-20 when it applies and collects the evidence the kept
// statements and findings cite.
func (s *session) finish(out *changereview.Reasoning) {
	if f, ev, ok := s.cr20(out.Statements); ok {
		out.Findings = append(out.Findings, f)
		s.evidence[ev.ID] = ev
	}
	out.Evidence = s.citedEvidence(*out)
}

// statements applies the extraction gate: an inspected statement that
// parsed and planned, from a file of the diff, within LineTolerance lines
// of an added line.
func (s *session) statements(out *changereview.Reasoning, in []finalStatement,
	aliases map[string]agentloop.Evidence) {
	seen := map[string]bool{}
	for _, st := range in {
		id := resolveAlias(st.EvidenceID, aliases)
		rec := s.inspections[id]
		file, inDiff := diffscan.Find(s.files, st.File)
		reason, why := "", ""
		switch {
		case len(out.Statements) >= MaxStatements:
			reason, why = "over_limit", fmt.Sprintf("more than %d statements", MaxStatements)
		case rec == nil:
			reason, why = "not_inspected", "it cites no inspect_statement result"
		case seen[id]:
			reason, why = "duplicate", "the same statement was submitted twice"
		case !rec.ins.Parsed:
			reason, why = "not_parsed", "it did not parse: "+rec.ins.ParseError
		case !rec.ins.Planned:
			reason, why = "not_planned", "it has no plan: "+rec.ins.PlanError
		case !inDiff:
			reason, why = "file_not_in_diff", "its file is not in the diff"
		case !file.NearAdded(st.Line, LineTolerance):
			reason, why = "line_not_added", fmt.Sprintf("line %d is not within %d lines "+
				"of an added line", st.Line, LineTolerance)
		}
		subject := fmt.Sprintf("statement at %s:%d", oneLine(st.File), st.Line)
		if reason != "" {
			s.dropStatement(out, st, rec, subject, reason, why)
			continue
		}
		seen[id] = true
		out.Statements = append(out.Statements, s.accepted(st, rec, file, id,
			len(s.a.Statements)+len(out.Statements)+1))
	}
}

func (s *session) dropStatement(out *changereview.Reasoning, st finalStatement,
	rec *inspection, subject, reason, why string) {
	what := st.EvidenceID
	if rec != nil {
		what = rec.sql
	}
	out.Dropped = append(out.Dropped, changereview.Dropped{Kind: "statement",
		Text: clipRunes(oneLine(subject+" "+what), 300), Reason: reason})
	out.NotDetermined = append(out.NotDetermined, changereview.NotDetermined{
		Subject: clipRunes(subject, 300), Reason: clipRunes(oneLine(why), 300)})
}

func (s *session) accepted(st finalStatement, rec *inspection, f diffscan.File, id string,
	index int) changereview.Statement {
	out := rec.ins.Statement
	out.Index, out.Origin, out.File, out.Line, out.EvidenceID = index, rec.origin, f.Path,
		st.Line, id
	out.Confidence = min(max(st.Confidence, 0), 1)
	out.InLoop = st.InLoop
	if orm := strings.ToLower(strings.TrimSpace(st.ORM)); ormArg.MatchString(orm) {
		out.ORM = orm
	}
	return out
}

func resolveAlias(ref string, aliases map[string]agentloop.Evidence) string {
	ref = strings.TrimSpace(ref)
	if e, ok := aliases[ref]; ok {
		return e.ID
	}
	return ref
}

// findings keeps the model's findings that cite evidence the run holds
// and whose every number appears there, clamped to warning.
func (s *session) findings(out *changereview.Reasoning, in []finalFinding,
	aliases map[string]agentloop.Evidence) {
	seen := map[string]bool{}
	kept := 0
	for _, mf := range in {
		title := clipRunes(oneLine(mf.Title), 200)
		text := clipRunes(oneLine(mf.Text), 1000)
		reason := ""
		var ids []string
		switch {
		case kept >= MaxFindings:
			reason = "over_limit"
		case title == "":
			reason = agentloop.DropEmpty
		case seen[strings.ToLower(title)]:
			reason = agentloop.DropDuplicate
		default:
			ids, reason = groundFinding(title, text, mf.EvidenceIDs, aliases)
		}
		if reason != "" {
			out.Dropped = append(out.Dropped, changereview.Dropped{Kind: "finding",
				Text: title, Reason: reason})
			continue
		}
		seen[strings.ToLower(title)] = true
		kept++
		out.Findings = append(out.Findings, changereview.Finding{Kind: kindOf(mf.Kind),
			Severity: severityOf(mf.Severity), Origin: changereview.OriginModel, Title: title,
			Text: text, EvidenceIDs: ids, Suggestion: s.suggestion(out, mf, title)})
	}
}

// groundFinding runs the citation filter and the investigator's number
// grounding on one finding; it returns the durable ids or why it fails.
func groundFinding(title, text string, cited []string,
	aliases map[string]agentloop.Evidence) ([]string, string) {
	claim := agentloop.Claim{Text: title + ". " + text, EvidenceIDs: cited}
	kept, dropped := agentloop.FilterClaims([]agentloop.Claim{claim}, aliases,
		sre.GroundClaim, 1)
	if len(kept) == 0 {
		if len(dropped) > 0 {
			return nil, dropped[0].Reason
		}
		return nil, agentloop.DropMalformed
	}
	return kept[0].EvidenceIDs, ""
}

func kindOf(kind string) string {
	if k := strings.TrimSpace(kind); kindArg.MatchString(k) {
		return k
	}
	return "model_finding"
}

func severityOf(severity string) string {
	if severity == changereview.SeverityInfo {
		return changereview.SeverityInfo
	}
	return changereview.SeverityWarning
}

// suggestion is a finding's fix when it fits MaxSuggestionBytes; a longer
// one is omitted and said so.
func (s *session) suggestion(out *changereview.Reasoning, mf finalFinding,
	title string) *changereview.Suggestion {
	if mf.Suggestion == nil || strings.TrimSpace(mf.Suggestion.Code) == "" {
		return nil
	}
	if len(mf.Suggestion.Code) > MaxSuggestionBytes {
		out.NotDetermined = append(out.NotDetermined, changereview.NotDetermined{
			Subject: "suggestion for " + title,
			Reason:  fmt.Sprintf("over %d bytes; omitted", MaxSuggestionBytes)})
		return nil
	}
	lang := strings.ToLower(strings.TrimSpace(mf.Suggestion.Language))
	if !languageArg.MatchString(lang) {
		lang = "text"
	}
	return &changereview.Suggestion{Language: lang, Code: mf.Suggestion.Code}
}

// intentText is the judged intent with the claims that support it.
func intentText(intent string, claims []agentloop.Claim) string {
	intent = strings.TrimSpace(intent)
	if intent == "" && len(claims) == 0 {
		return ""
	}
	if !intents[intent] {
		intent = IntentUnclear
	}
	parts := make([]string, 0, len(claims))
	for _, c := range claims {
		parts = append(parts, c.Text+" ["+strings.Join(c.EvidenceIDs, ", ")+"]")
	}
	if len(parts) == 0 {
		return intent
	}
	return intent + ": " + strings.Join(parts, "; ")
}

// modelNotDetermined keeps at most MaxModelNotDetermined items without
// digits (a number there would be ungrounded).
func modelNotDetermined(items []string) []changereview.NotDetermined {
	var out []changereview.NotDetermined
	for _, item := range items {
		item = clipRunes(oneLine(item), 300)
		if item == "" || strings.IndexFunc(item, unicode.IsDigit) >= 0 {
			continue
		}
		if len(out) == MaxModelNotDetermined {
			break
		}
		out = append(out, changereview.NotDetermined{Subject: item,
			Reason: ModelNotDeterminedReason})
	}
	return out
}
