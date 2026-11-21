package cases

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/verify"
)

const (
	prModelTimeout   = 30 * time.Second
	maxPRModelOutput = 1200
	prModelSystem    = `You explain a database change to the engineer who reviews its ` +
		`pull request. Respond with ONLY a JSON object {"paragraph":"..."}: one plain ` +
		`paragraph of at most 120 words saying what the change does. Use only the facts ` +
		`given. Name no table, column, file, URL or number that is not in the facts.`
)

var (
	// identifierToken matches what a paragraph may only name when the facts
	// do: qualified names, snake_case words and numbers of 3+ digits.
	identifierToken = regexp.MustCompile(
		`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+|` +
			`[A-Za-z0-9]*_[A-Za-z0-9_]+|[0-9]{3,}`)
	urlToken = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://|\bwww\.`)
)

// prFacts are the redacted model-tier facts: no SQL text, no free text
// from the finding, only names pg_sage put in the PR itself.
type prFacts struct {
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	RiskTier string   `json:"risk_tier"`
	Files    []string `json:"files"`
	CallSite string   `json:"call_site,omitempty"`
	Effect   string   `json:"expected_effect"`
}

func factsFor(c PRCandidate, ch prChange) string {
	f := prFacts{Kind: c.Kind, Title: c.Title, RiskTier: c.RiskTier, CallSite: ch.callSite,
		Effect: "not measured yet"}
	for _, file := range ch.files {
		f.Files = append(f.Files, file.Path)
	}
	if best, ok := verify.HighestPassed(c.Ladder); ok {
		f.Effect = ladderLine(best)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// paragraph is the model's plain-words paragraph, or the template when the
// model is off, fails, or answers anything unparseable or ungrounded.
func (m *PRMode) paragraph(ctx context.Context, c PRCandidate, ch prChange) string {
	template := templateParagraph(c, ch)
	if m.deps.Model == nil {
		return template
	}
	ctx, cancel := context.WithTimeout(ctx, prModelTimeout)
	defer cancel()
	facts := factsFor(c, ch)
	raw, err := m.deps.Model.Chat(ctx, prModelSystem, facts)
	if err != nil {
		return template
	}
	if p, ok := parseParagraph(raw); ok && paragraphGrounded(p, facts) {
		return p
	}
	return template
}

func parseParagraph(raw string) (string, bool) {
	var out struct {
		Paragraph string `json:"paragraph"`
	}
	if err := json.Unmarshal([]byte(llm.StripJSON(raw, llm.JSONObject)), &out); err != nil {
		return "", false
	}
	p := strings.Join(strings.Fields(out.Paragraph), " ")
	if p == "" || len(p) > maxPRModelOutput {
		return "", false
	}
	return p, true
}

// paragraphGrounded refuses a URL, or an identifier the facts lack.
func paragraphGrounded(p, facts string) bool {
	if urlToken.MatchString(p) {
		return false
	}
	lower := strings.ToLower(facts)
	for _, tok := range identifierToken.FindAllString(p, -1) {
		if !strings.Contains(lower, strings.ToLower(tok)) {
			return false
		}
	}
	return true
}

func templateParagraph(c PRCandidate, ch prChange) string {
	if c.Kind == PRKindRewrite {
		return fmt.Sprintf("This replaces the SQL text at %s with a rewrite that passed "+
			"pg_sage's equivalence check: %s. It changes the application, not the database.",
			ch.callSite, c.Title)
	}
	path := ""
	if len(ch.files) > 0 {
		path = ch.files[0].Path
	}
	return fmt.Sprintf("This adds one new migration file, %s: %s. pg_sage does not run "+
		"it; your deploy applies it after a person merges this PR.", path, c.Title)
}
