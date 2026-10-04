package firstlook

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Summary bounds.
const (
	maxSummaryChars  = 600
	maxSummaryItems  = 30
	summaryMaxTokens = 400
)

// Chatter is the model client (llm.Client).
type Chatter interface {
	Chat(ctx context.Context, system, user string, maxTokens int) (string, int, error)
}

// Summarizer asks the model for a short plain-language summary of a first
// look. The findings exist without it; the summary only adds a headline.
type Summarizer struct{ client Chatter }

// NewSummarizer summarizes through client.
func NewSummarizer(client Chatter) *Summarizer { return &Summarizer{client: client} }

const summarySystemPrompt = `You are pg_sage, an AI DBA. You get the catalog-only ` +
	`first look of a PostgreSQL database: deterministic findings, each with a severity. ` +
	`Write at most three short sentences for the operator: what matters most first, ` +
	`then what can wait. Do not invent findings, numbers or object names. Respond with ` +
	`only a JSON object: {"summary": "..."}
` + llm.UntrustedDataRule

// Summarize returns the model's summary of r, or "" without asking when r
// has no items. A reply that is not a summary is ErrSummaryOutput.
func (s *Summarizer) Summarize(ctx context.Context, r Report) (string, error) {
	if s == nil || s.client == nil {
		return "", ErrNoModel
	}
	if len(r.Items) == 0 {
		return "", nil
	}
	reply, _, err := s.client.Chat(ctx, summarySystemPrompt, summaryPrompt(r),
		summaryMaxTokens)
	if err != nil {
		return "", fmt.Errorf("first look summary: %w", err)
	}
	var out struct {
		Summary string `json:"summary"`
	}
	if err := llm.ParseJSON(reply, llm.JSONObject, &out); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSummaryOutput, err)
	}
	summary := strings.Join(strings.Fields(out.Summary), " ")
	if summary == "" {
		return "", fmt.Errorf("%w: empty summary", ErrSummaryOutput)
	}
	return clipWords(summary, maxSummaryChars), nil
}

// summaryPrompt lists the findings as untrusted data: object names come
// from the database and may carry text aimed at the model.
func summaryPrompt(r Report) string {
	var b strings.Builder
	for i, it := range r.Items {
		if i == maxSummaryItems {
			fmt.Fprintf(&b, "... and %d more\n", len(r.Items)-i)
			break
		}
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", it.Severity, it.Title, it.Rule)
	}
	return fmt.Sprintf("First look of database %q: %d findings.\n%s", r.Database,
		len(r.Items), llm.UntrustedData("first_look_findings", b.String()))
}

// clipWords cuts s to at most n bytes at a word boundary.
func clipWords(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n-3], " ")
	if cut <= 0 {
		cut = n - 3
	}
	return strings.TrimSpace(s[:cut]) + "..."
}
