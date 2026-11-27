package briefing

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Payload is one briefing's data (llm-08, llm-06). Generate builds it once: it is
// the LLM input, it is stored as sage.briefings.content_json, and its
// recommendations are the only statements the LLM rewrite may repeat.
type Payload struct {
	Findings  []FindingRef     `json:"findings"`
	TotalOpen int              `json:"total_open"`
	System    map[string]any   `json:"system,omitempty"`
	Actions   []map[string]any `json:"actions"`
	// Window is the span the briefing covers; Actions are those executed in
	// it (llm-06).
	Window Window `json:"window"`
	// OpenBySeverity counts every open finding by severity, not only the
	// capped Findings list.
	OpenBySeverity map[string]int `json:"open_by_severity,omitempty"`
	// Digest is what the briefing sends (ux-02b); never part of the prompt.
	Digest *Digest `json:"digest,omitempty"`
	// database is the fleet database the case links open on the Cases page.
	database string
}

// FindingRef is one open finding as the briefing presents it.
type FindingRef struct {
	ID int64 `json:"id"`
	// CaseKey is the finding's case on the dashboard Cases page. It is
	// stored, not sent to the LLM.
	CaseKey          string  `json:"case_key,omitempty"`
	Category         string  `json:"category"`
	Severity         string  `json:"severity"`
	Title            string  `json:"title"`
	ObjectIdentifier *string `json:"object_identifier"`
	OccurrenceCount  int     `json:"occurrence_count"`
	// RiskTier is the case projector's tier for RecommendedSQL.
	RiskTier       string `json:"risk_tier,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	// RecommendedSQL is redacted by llm.SanitizeForLLM: no literal or
	// comment reaches the LLM or the stored payload.
	RecommendedSQL string `json:"recommended_sql,omitempty"`
	// rawSQL is the unredacted recommendation, used only for grounding.
	rawSQL string
}

// findingRow is one element of the gatherFindings JSON array.
type findingRow struct {
	ID               int64          `json:"id"`
	Category         string         `json:"category"`
	Severity         string         `json:"severity"`
	Title            string         `json:"title"`
	ObjectType       string         `json:"object_type"`
	ObjectIdentifier *string        `json:"object_identifier"`
	RuleID           string         `json:"rule_id"`
	OccurrenceCount  int            `json:"occurrence_count"`
	Recommendation   string         `json:"recommendation"`
	RecommendedSQL   string         `json:"recommended_sql"`
	Detail           map[string]any `json:"detail"`
}

// buildPayload decodes the gathered findings, system and action JSON.
// database names the fleet database, so case keys match the Cases page.
// System and action JSON that does not decode is left out, as the
// structured report leaves it out; findings that do not decode are an error.
func buildPayload(findings string, totalOpen int, system, actions, database string) (
	Payload, error,
) {
	var rows []findingRow
	if err := json.Unmarshal([]byte(findings), &rows); err != nil {
		return Payload{}, fmt.Errorf("decode briefing findings: %w", err)
	}
	p := Payload{Findings: make([]FindingRef, 0, len(rows)), TotalOpen: totalOpen,
		database: database}
	for _, r := range rows {
		p.Findings = append(p.Findings, r.ref(database))
	}
	if err := json.Unmarshal([]byte(system), &p.System); err != nil {
		p.System = nil
	}
	if err := json.Unmarshal([]byte(actions), &p.Actions); err != nil || p.Actions == nil {
		p.Actions = []map[string]any{}
	}
	return p, nil
}

func (r findingRow) ref(database string) FindingRef {
	object := ""
	if r.ObjectIdentifier != nil {
		object = *r.ObjectIdentifier
	}
	key := cases.IdentityKeyForFinding(cases.SourceFinding{
		ID: strconv.FormatInt(r.ID, 10), DatabaseName: database, Category: r.Category,
		ObjectType: r.ObjectType, ObjectIdentifier: object, RuleID: r.RuleID,
		Detail: r.Detail,
	})
	return FindingRef{
		ID: r.ID, CaseKey: key, Category: r.Category, Severity: r.Severity,
		Title: r.Title, ObjectIdentifier: r.ObjectIdentifier,
		OccurrenceCount: r.OccurrenceCount,
		RiskTier:        cases.RiskTierForSQL(r.RecommendedSQL, executor.ClassifySQL),
		Recommendation:  r.Recommendation,
		RecommendedSQL:  llm.SanitizeForLLM(r.RecommendedSQL),
		rawSQL:          r.RecommendedSQL,
	}
}

// promptJSON is the payload as the LLM sees it: without case keys.
func (p Payload) promptJSON() (string, error) {
	view := p
	view.Digest = nil
	view.Findings = make([]FindingRef, len(p.Findings))
	for i, f := range p.Findings {
		f.CaseKey = ""
		view.Findings[i] = f
	}
	b, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("encode briefing payload: %w", err)
	}
	return string(b), nil
}

// byID indexes the payload's findings by id.
func (p Payload) byID() map[int64]FindingRef {
	out := make(map[int64]FindingRef, len(p.Findings))
	for _, f := range p.Findings {
		out[f.ID] = f
	}
	return out
}

// allowedStatements is every recommended statement, raw and redacted, in
// normalized form.
func (p Payload) allowedStatements() map[string]bool {
	out := make(map[string]bool)
	for _, f := range p.Findings {
		for _, sql := range []string{f.rawSQL, f.RecommendedSQL} {
			for _, piece := range llm.SplitStatements(sql) {
				if n := normalizeStatement(piece); n != "" {
					out[n] = true
				}
			}
		}
	}
	return out
}
