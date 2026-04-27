package tuner

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

const llmMaxTokens = 4096

// LLMPrescription is the structured response from the LLM.
type LLMPrescription struct {
	HintDirective    string  `json:"hint_directive"`
	Rationale        string  `json:"rationale"`
	Confidence       float64 `json:"confidence"`
	SuggestedRewrite string  `json:"suggested_rewrite"`
	RewriteRationale string  `json:"rewrite_rationale"`
}

// llmPrescribe calls the LLM for hint reasoning, with fallback.
func llmPrescribe(
	ctx context.Context,
	client *llm.Client,
	fallback *llm.Client,
	qctx QueryContext,
	logFn func(string, string, ...any),
) ([]Prescription, error) {
	system := TunerSystemPrompt()
	prompt := FormatTunerPrompt(qctx)

	resp, _, err := client.Chat(ctx, system, prompt, llmMaxTokens)
	if err != nil && fallback != nil {
		logFn("tuner",
			"primary LLM failed, trying fallback: %v", err)
		resp, _, err = fallback.Chat(
			ctx, system, prompt, llmMaxTokens,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("llm chat: %w", err)
	}

	recs, err := parseLLMPrescriptions(resp)
	if err != nil {
		logFn("tuner",
			"LLM response parse error: %v (response: %.200s)",
			err, resp)
		return nil, fmt.Errorf("parse llm response: %w", err)
	}

	return convertPrescriptions(recs, logFn), nil
}

func convertPrescriptions(
	recs []LLMPrescription,
	logFn func(string, string, ...any),
) []Prescription {
	var out []Prescription
	for _, r := range recs {
		if !validateHintSyntax(r.HintDirective) {
			logFn("tuner",
				"rejecting invalid LLM hint: %s", r.HintDirective)
			continue
		}
		out = append(out, Prescription{
			Symptom:          "llm_recommended",
			HintDirective:    r.HintDirective,
			Rationale:        r.Rationale,
			SuggestedRewrite: r.SuggestedRewrite,
			RewriteRationale: r.RewriteRationale,
		})
	}
	return out
}

func parseLLMPrescriptions(
	response string,
) ([]LLMPrescription, error) {
	var recs []LLMPrescription
	if err := llm.ParseJSON(response, llm.JSONArray, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// stripToJSON extracts the JSON array from a response that may
// contain thinking text, markdown fences, or other non-JSON
// content. Delegates to the canonical llm.StripJSON.
func stripToJSON(s string) string {
	return llm.StripJSON(s, llm.JSONArray)
}

// validHintTokens are the allowed pg_hint_plan directive prefixes.
var validHintTokens = []string{
	"Set(", "HashJoin(", "MergeJoin(", "NestLoop(",
	"IndexScan(", "IndexOnlyScan(", "SeqScan(", "NoSeqScan(",
	"Parallel(", "NoParallel(",
	"BitmapScan(", "NoBitmapScan(",
	"NoIndexScan(", "NoNestLoop(", "NoHashJoin(", "NoMergeJoin(",
}

// dangerousPatterns rejects SQL injection attempts.
var dangerousPatterns = regexp.MustCompile(
	`(?i)(;|--|\b(DROP|DELETE|INSERT|ALTER|CREATE|TRUNCATE|UPDATE|GRANT|REVOKE)\b)`,
)

// validateHintSyntax checks a hint string contains only valid
// pg_hint_plan tokens and no dangerous SQL.
func validateHintSyntax(hint string) bool {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return false
	}
	if dangerousPatterns.MatchString(hint) {
		return false
	}
	// Every non-whitespace token must start with a known prefix.
	// Split on ")  " boundaries to isolate directives.
	parts := splitHintDirectives(hint)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !hasValidPrefix(p) {
			return false
		}
	}
	return true
}

func hasValidPrefix(s string) bool {
	for _, tok := range validHintTokens {
		if strings.HasPrefix(s, tok) {
			return true
		}
	}
	return false
}

// splitHintDirectives splits a combined hint string like
// "Set(work_mem \"256MB\") HashJoin(t1 t2)" into individual
// directive strings.
func splitHintDirectives(hint string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, ch := range hint {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parts = append(parts, hint[start:i+1])
				start = i + 1
			}
		}
	}
	// Trailing text (shouldn't happen in valid hints)
	if trail := strings.TrimSpace(hint[start:]); trail != "" {
		parts = append(parts, trail)
	}
	return parts
}
